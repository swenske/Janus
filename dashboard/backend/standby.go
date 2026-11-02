package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/backup"
	"github.com/swenske/Janus/dashboard/backend/internal/ha"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
)

// The standby's side of the warm standby (internal/ha): a Controller
// paired with an active runs none of its loops and opens none of its
// stores. It pulls a copy of the active every minute, answers the
// active's questions on its registration port, shows a page saying what
// it is, and waits to be promoted - `dashboardd promote` on its host, or
// the active's switchover.

// standbyPullEvery is how often a standby copies the active
// (JANUS_CONTROLLER_STANDBY_PULL: a duration, for the tests).
func standbyPullEvery() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("JANUS_CONTROLLER_STANDBY_PULL")); err == nil && d >= time.Second {
		return d
	}
	return time.Minute
}

// standby is a Controller running as a standby.
type standby struct {
	ha            *ha.Store
	dataDir       string
	masterKeyFile string // the flag: empty = <data>/master.key
	masterKeyPath string
	promoting     atomic.Bool
}

// controlSocket is where `dashboardd promote` finds the running standby.
func controlSocket(dataDir string) string { return filepath.Join(dataDir, ha.Dir, "control.sock") }

// runStandby is the Controller as a standby, for as long as it runs.
func runStandby(s *standby, addr, registerAddr, tlsCert, tlsKey string) {
	if n, err := setAside(s.dataDir, s.masterKeyPath, time.Now()); err != nil {
		log.Fatalf("standby: setting the data directory aside: %v", err)
	} else if n != "" {
		log.Printf("standby: what this Controller held as the active is set aside in %s", n)
	}
	st := s.ha.State()
	log.Printf("dashboardd %s: the standby of %s, epoch %d - copying it every %s; promote with dashboardd promote", version, peerName(st), st.Epoch, standbyPullEvery())

	cert := s.ha.Certificate()
	if tlsCert != "" && tlsKey != "" {
		c, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			log.Fatalf("standby: -tls-cert/-tls-key: %v", err)
		}
		cert = &c
	}
	// The registration port: the active's questions. Nodes aren't served:
	// they find the active.
	peerMux := http.NewServeMux()
	peerMux.HandleFunc("GET /ha/state", s.peer(func(w http.ResponseWriter, _ *http.Request) {
		st := s.ha.State()
		writeJSON(w, http.StatusOK, ha.PeerState{Role: ha.Standby, Epoch: st.Epoch, Version: version, Name: haName()})
	}))
	peerMux.HandleFunc("POST /ha/promote", s.peer(func(w http.ResponseWriter, _ *http.Request) {
		if s.promoting.Load() {
			writeError(w, http.StatusConflict, "promoting already")
			return
		}
		go func() {
			if msg, err := s.promote(context.Background(), false); err != nil {
				log.Printf("standby: the switchover failed: %v", err)
			} else {
				log.Print(msg)
				go restartSelf()
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"message": "taking over"})
	}))
	peerMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "this Controller is a standby: the active answers")
	})
	serve(registerAddr, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*s.ha.Certificate()}}, peerMux)

	// The main port: a page saying what this is, and keepalived's check.
	mainMux := http.NewServeMux()
	mainMux.HandleFunc("GET /api/ha/active", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"role": ha.Standby, "epoch": s.ha.State().Epoch})
	})
	mainMux.HandleFunc("GET /api/ha", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.status())
	})
	mainMux.HandleFunc("GET /{$}", s.page)
	mainMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "this Controller is a standby: the active answers")
	})
	serve(addr, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}, securityHeaders(mainMux))

	// `dashboardd promote` on this host.
	sock := controlSocket(s.dataDir)
	_ = os.Remove(sock)
	if ln, err := net.Listen("unix", sock); err != nil {
		log.Printf("standby: the control socket: %v - dashboardd promote can't reach this process", err)
	} else {
		ctl := http.NewServeMux()
		ctl.HandleFunc("POST /promote", func(w http.ResponseWriter, r *http.Request) {
			msg, err := s.promote(r.Context(), r.URL.Query().Get("force") == "1")
			if err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			log.Print(msg)
			writeJSON(w, http.StatusOK, map[string]string{"message": msg})
			go restartSelf()
		})
		go func() { _ = (&http.Server{Handler: ctl, ReadHeaderTimeout: 10 * time.Second}).Serve(ln) }()
	}

	s.pullLoop()
}

func serve(addr string, tlsConfig *tls.Config, h http.Handler) {
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		log.Fatalf("standby: listen on %s: %v", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("standby: %s: %v", addr, err)
		}
	}()
}

func (s *standby) peer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ha.Authorized(bearerOf(r)) {
			writeError(w, http.StatusUnauthorized, "not this Controller's peer")
			return
		}
		next(w, r)
	}
}

// pullLoop copies the active every standbyPullEvery, until a promotion.
func (s *standby) pullLoop() {
	for {
		if !s.promoting.Load() {
			if err := s.pull(context.Background()); err != nil {
				log.Printf("standby: %v", err)
			}
		}
		time.Sleep(standbyPullEvery())
	}
}

// pull takes a copy of the active, or says why not.
func (s *standby) pull(ctx context.Context) error {
	c := s.ha.Client()
	if c == nil {
		return errors.New("not paired")
	}
	c.Version = version
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	st, err := c.State(ctx)
	if err != nil {
		_ = s.ha.NotePull(now, false, 0, err)
		return fmt.Errorf("the active %s: %w", peerName(s.ha.State()), err)
	}
	if st.Role != ha.Active {
		err := fmt.Errorf("the peer %s is a %s too, epoch %d: promote one of the two", st.Name, st.Role, st.Epoch)
		_ = s.ha.NotePull(now, true, st.Epoch, err)
		return err
	}
	if newerThanThis(st.Version) {
		err := fmt.Errorf("the active runs %s, newer than this standby (%s): update the standby - it keeps no copy it may not read", st.Version, version)
		_ = s.ha.NotePull(now, true, st.Epoch, err)
		return err
	}
	archive, h, err := c.Do(ctx, http.MethodGet, "/ha/replica", nil, ha.MaxReplica)
	if err != nil {
		_ = s.ha.NotePull(now, true, st.Epoch, err)
		return err
	}
	if s.promoting.Load() {
		return nil
	}
	return s.ha.SaveReplica(archive, ha.ReplicaFrom(h, now))
}

// status is what the standby's main port says to anyone: no address, no
// version - `dashboardd ha status` on its host says more.
func (s *standby) status() map[string]any {
	st := s.ha.State()
	p := s.ha.Pulls()
	out := map[string]any{"role": ha.Standby, "epoch": st.Epoch, "last_contact": p.LastContact}
	if p.Last != nil {
		out["last_copy"] = p.Last.At
	}
	if p.Error != "" {
		out["error"] = p.Error
	}
	if st.Peer != nil {
		out["active"] = st.Peer.Name
	}
	return out
}

var standbyPage = template.Must(template.New("standby").Funcs(template.FuncMap{
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="30"><title>Janus Controller - standby</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem;color:#1f2933;background:#fff}
@media (prefers-color-scheme:dark){body{color:#e4e7eb;background:#111418}}code{font-size:.9em}dt{font-weight:600}dd{margin:0 0 .6rem}.err{color:#c0392b}</style></head>
<body><h1>Janus Controller - standby</h1>
<p>This Controller is the standby of <b>{{.Active}}</b>: it copies it and waits. Sign in on the active.</p>
<dl><dt>Epoch</dt><dd>{{.Epoch}}</dd>
<dt>Last copy</dt><dd>{{if .Last}}{{ago .Last.At}}{{else}}none yet{{end}}</dd>
<dt>The active last answered</dt><dd>{{ago .Pulls.LastContact}}</dd>
{{if .Pulls.Error}}<dt>Last pull</dt><dd class="err">{{.Pulls.Error}}</dd>{{end}}</dl>
<p>To take over: <code>dashboardd promote</code> on this host, or the active's switchover (Settings, High availability).</p>
</body></html>`))

func (s *standby) page(w http.ResponseWriter, _ *http.Request) {
	st := s.ha.State()
	p := s.ha.Pulls()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	active := "?"
	if st.Peer != nil {
		active = st.Peer.Name
	}
	_ = standbyPage.Execute(w, map[string]any{"Active": active, "Epoch": st.Epoch, "Last": p.Last, "Pulls": p})
}

// --- promotion ---

// promote makes this standby the active: the active's last copy - handed
// over by the active, which steps down, or the last one pulled when the
// active is gone - restored in the data directory, one epoch above
// anything seen, the witness's included - which it claims first (the
// copy says where it is): without the active's handover, a witness it
// can't write refuses the promotion unless forced. The caller then
// starts the Controller again (or, with none running, the operator does).
func (s *standby) promote(ctx context.Context, force bool) (string, error) {
	if !s.promoting.CompareAndSwap(false, true) {
		return "", errors.New("promoting already")
	}
	ok := false
	defer func() {
		if !ok {
			s.promoting.Store(false)
		}
	}()
	st := s.ha.State()
	if st.Role != ha.Standby {
		return "", errors.New("this Controller isn't a standby")
	}
	if err := writable(s.masterKeyPath); err != nil {
		return "", fmt.Errorf("the master key's file must be writable - the active's goes there: %w", err)
	}
	pulls := s.ha.Pulls()
	var archive []byte
	var rep ha.Replica
	how := "handed over by the active"
	handedOver := false
	if c := s.ha.Client(); c != nil {
		hctx, cancel := context.WithTimeout(ctx, time.Minute)
		data, h, err := c.Do(hctx, http.MethodPost, "/ha/stepdown", nil, ha.MaxReplica)
		cancel()
		if err == nil {
			archive, rep, handedOver = data, ha.ReplicaFrom(h, time.Now().UTC()), true
		} else {
			if merr := ha.MayPromote(pulls.LastContact, time.Now(), force); merr != nil {
				return "", fmt.Errorf("the active didn't hand over (%v); %w", err, merr)
			}
			how = "the last one pulled - the active didn't hand over: " + err.Error()
		}
	}
	if archive == nil {
		var err error
		if archive, rep, err = s.ha.LastReplica(); err != nil {
			return "", err
		}
	}
	if newerThanThis(rep.Version) {
		return "", fmt.Errorf("the copy is of version %s, newer than this Controller (%s): update it first", rep.Version, version)
	}
	epoch := max(st.Epoch, pulls.PeerEpoch, rep.Epoch) + 1
	// The witness first, before anything is touched: an active the
	// promotion couldn't reach learns it there that it's replaced.
	witness := "no witness"
	w, err := witnessOfCopy(archive)
	if err == nil {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var got uint64
		got, err = s.ha.ClaimWitness(wctx, w, epoch, haName())
		cancel()
		if err == nil {
			epoch, witness = got, "the witness "+w.Where()+" holds it"
		} else {
			err = fmt.Errorf("the witness %s: %w", w.Where(), err)
		}
	}
	switch {
	case errors.Is(err, errNoWitness):
	case err != nil && !handedOver && !force:
		return "", fmt.Errorf("%v - the active couldn't learn it's replaced: make sure it's stopped, then promote -force", err)
	case err != nil:
		witness = "not written in the witness: " + err.Error()
	}
	if err := clearDataDir(s.dataDir, s.masterKeyPath); err != nil {
		return "", err
	}
	n, err := restoreFiles(archive, s.dataDir, s.masterKeyPath)
	if err != nil {
		return "", fmt.Errorf("restoring the copy: %w - the data directory may be half restored", err)
	}
	key, err := secrets.LoadOrCreate(s.masterKeyFile, s.dataDir)
	if err != nil {
		return "", fmt.Errorf("the active's master key: %w", err)
	}
	if err := s.ha.Reseal(key); err != nil {
		return "", err
	}
	if err := noteRestored(s.dataDir, &backup.Manifest{Created: rep.Created, Controller: "the active " + peerName(st), Version: rep.Version}); err != nil {
		log.Printf("standby: %v", err)
	}
	if err := s.ha.SetRole(ha.Active, epoch); err != nil {
		return "", err
	}
	ok = true
	return fmt.Sprintf("standby: promoted - the active now, epoch %d (%s), on the copy of %s (%s, version %s, %d files)", epoch, witness, rep.Created.Format(time.RFC3339), how, rep.Version, n), nil
}

// writable checks path may be written - created if missing.
func writable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// keptInDataDir is what a standby's data directory keeps of its own: its
// HA files, its lock, its master key when it lives there.
func keptInDataDir(dataDir, masterKeyPath, name string) bool {
	if name == ha.Dir || name == dataLockFile {
		return true
	}
	rel, err := filepath.Rel(dataDir, masterKeyPath)
	return err == nil && rel == name
}

// setAside moves what a data directory holds besides keptInDataDir into
// ha/previous/<time>: a Controller that became a standby keeps what it
// held as the active, out of the way. The directory made, or "".
func setAside(dataDir, masterKeyPath string, now time.Time) (string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return "", err
	}
	dest := ""
	for _, e := range entries {
		if keptInDataDir(dataDir, masterKeyPath, e.Name()) {
			continue
		}
		if dest == "" {
			dest = filepath.Join(dataDir, ha.Dir, "previous", now.UTC().Format("20060102T150405Z"))
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return "", err
			}
		}
		if err := os.Rename(filepath.Join(dataDir, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// clearDataDir empties a standby's data directory before a copy is
// restored in it - setAside left it so; what a crash left goes.
func clearDataDir(dataDir, masterKeyPath string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if keptInDataDir(dataDir, masterKeyPath, e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// --- dashboardd standby | promote | ha status ---

// standbyCommand pairs a new Controller with an active as its standby:
// `dashboardd standby -peer HOST:8443 -pair TOKEN`, the token from the
// active's Settings, High availability. The Controller then started on
// this data directory runs as the standby.
func standbyCommand(args []string) {
	fs := flag.NewFlagSet("standby", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/data", "the standby's data directory - a new one")
	masterKeyFile := fs.String("master-key-file", envOr("JANUS_CONTROLLER_MASTER_KEY_FILE", ""), "the master key (default: <data-dir>/master.key) - writable: a promotion puts the active's there")
	peerAddr := fs.String("peer", "", "the active's registration port, HOST:PORT - its own address, not one that moves with the active (a VIP)")
	token := fs.String("pair", "", "the pairing token from the active's Settings, High availability")
	name := fs.String("name", haName(), "what the active calls this standby")
	self := fs.String("address", suggestRegisterAddress(envOr("JANUS_CONTROLLER_ADVERTISE_ADDRESS", ""), ":8443"), "this standby's registration port as the active reaches it, HOST:PORT")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: dashboardd standby [-data-dir DIR] [-master-key-file FILE] -peer HOST:PORT -pair TOKEN [-name NAME] [-address HOST:PORT]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *peerAddr == "" || *token == "" || *self == "" {
		fs.Usage()
		os.Exit(2)
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "standby:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fail(err)
	}
	if err := lockDataDir(*dataDir); err != nil {
		fail(err)
	}
	if st, err := auth.Open(*dataDir); err != nil {
		fail(err)
	} else if !st.SetupRequired() {
		fail(fmt.Errorf("%s has accounts: a standby starts on a new data directory - the active's copy fills it", *dataDir))
	}
	key, err := secrets.LoadOrCreate(*masterKeyFile, *dataDir)
	if err != nil {
		fail(err)
	}
	store, err := ha.Open(filepath.Join(*dataDir, ha.Dir), key)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := pairStandby(ctx, store, *peerAddr, *token, *name, *self)
	if err != nil {
		fail(err)
	}
	s := &standby{ha: store, dataDir: *dataDir, masterKeyFile: *masterKeyFile, masterKeyPath: key.Path}
	first := "no first copy yet"
	if err := s.pull(context.Background()); err != nil {
		first = "no first copy yet: " + err.Error()
	} else if p := store.Pulls(); p.Last != nil {
		first = fmt.Sprintf("a first copy of %d bytes", p.Last.Size)
	}
	fmt.Printf("Paired with %s (%s, version %s) as its standby, epoch %d - %s.\nStart the Controller on %s: it runs as the standby, and tells the active it's there at %s.\n", resp.Name, *peerAddr, resp.Version, resp.Epoch, first, *dataDir, *self)
}

// pairStandby pairs store's Controller with the active at peerAddr, by
// the token its page made: this Controller is name, its registration
// port at self as the active reaches it.
func pairStandby(ctx context.Context, store *ha.Store, peerAddr, token, name, self string) (haPairResponse, error) {
	var resp haPairResponse
	if st := store.State(); st.Peer != nil {
		return resp, fmt.Errorf("paired already with %s", peerName(st))
	}
	fp, secret, err := ha.ParseToken(token)
	if err != nil {
		return resp, err
	}
	//nolint:gosec // G117: the token's one-time secret, sent to the active its certificate pins
	body, err := json.Marshal(haPairRequest{Secret: secret, Name: name, Address: self, CertPEM: string(store.CertPEM()), Version: version})
	if err != nil {
		return resp, err
	}
	data, _, err := ha.NewClient(peerAddr, fp, "").Do(ctx, http.MethodPost, "/ha/pair", body, 1<<16)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return resp, err
	}
	shared, err := base64.RawURLEncoding.DecodeString(resp.Secret)
	if err != nil || len(shared) < 16 {
		return resp, errors.New("the active's answer carries no secret")
	}
	return resp, store.SetPeer(ha.Peer{Name: resp.Name, Address: peerAddr, CertSHA256: fp, PairedAt: time.Now().UTC()}, shared, ha.Standby, resp.Epoch)
}

// promoteCommand is `dashboardd promote [-force]`: the standby running on
// this data directory takes over - through its control socket -, or,
// with none running, the data directory is promoted for the next start.
func promoteCommand(args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/data", "the standby's data directory")
	masterKeyFile := fs.String("master-key-file", envOr("JANUS_CONTROLLER_MASTER_KEY_FILE", ""), "its master key (default: <data-dir>/master.key)")
	force := fs.Bool("force", false, "take over from the last copy even though the active answered less than 2 minutes ago, or the witness can't be written - it may still run: stop it first")
	_ = fs.Parse(args)
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "promote:", err)
		os.Exit(1)
	}
	q := ""
	if *force {
		q = "?force=1"
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", controlSocket(*dataDir))
	}}}
	resp, err := hc.Post("http://standby/promote"+q, "application/json", nil)
	if err == nil {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var out map[string]string
		_ = json.Unmarshal(data, &out)
		if resp.StatusCode != http.StatusOK {
			fail(errors.New(strings.TrimSpace(string(data))))
		}
		fmt.Println(out["message"] + " - the Controller starts again as the active.")
		return
	}
	// No standby answering: promote the data directory itself.
	if err := lockDataDir(*dataDir); err != nil {
		fail(fmt.Errorf("the standby doesn't answer on %s, and %w", controlSocket(*dataDir), err))
	}
	key, err := secrets.LoadOrCreate(*masterKeyFile, *dataDir)
	if err != nil {
		fail(err)
	}
	store, err := ha.Open(filepath.Join(*dataDir, ha.Dir), key)
	if err != nil {
		fail(err)
	}
	s := &standby{ha: store, dataDir: *dataDir, masterKeyFile: *masterKeyFile, masterKeyPath: key.Path}
	msg, err := s.promote(context.Background(), *force)
	if err != nil {
		fail(err)
	}
	fmt.Println(msg + " - start the Controller.")
}

// haCommand is `dashboardd ha status`: where this data directory stands -
// read beside a running Controller.
func haCommand(args []string) {
	fs := flag.NewFlagSet("ha", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/data", "the Controller's data directory")
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprintln(os.Stderr, "usage: dashboardd ha status [-data-dir DIR]")
		os.Exit(2)
	}
	_ = fs.Parse(args[1:])
	st, p, err := ha.ReadPulls(filepath.Join(*dataDir, ha.Dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ha:", err)
		os.Exit(1)
	}
	fmt.Printf("role:   %s\nepoch:  %d\n", st.Role, st.Epoch)
	if st.Peer == nil {
		fmt.Println("peer:   none")
		return
	}
	fmt.Printf("peer:   %s (%s), paired %s\n", st.Peer.Name, st.Peer.Address, st.Peer.PairedAt.Format(time.RFC3339))
	if st.Role != ha.Standby {
		return
	}
	if p.Last != nil {
		fmt.Printf("copy:   %s ago - version %s, epoch %d, %d bytes\n", time.Since(p.Last.At).Round(time.Second), p.Last.Version, p.Last.Epoch, p.Last.Size)
	} else {
		fmt.Println("copy:   none yet")
	}
	if !p.LastContact.IsZero() {
		fmt.Printf("active: answered %s ago\n", time.Since(p.LastContact).Round(time.Second))
	}
	if p.Error != "" {
		fmt.Printf("error:  %s\n", p.Error)
	}
}
