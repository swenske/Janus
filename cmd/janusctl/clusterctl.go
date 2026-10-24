package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/pki"
)

// janusctl cluster as a cluster's orchestrator, for a fleet without a
// Controller (docs/cluster.md phase 3, docs/guide/clusters.md "Without a
// Controller"): the Controller's engine (internal/clusters) run from this
// machine, on the fleet context's nodes. The clusters live in the
// context's directory - versions, state, journal, and the cluster's CA,
// its key 0600 like the fleet's issuing key. The members' locks are
// advisory: an admin's certificate gets through with the cluster's id,
// which this orchestrator sends.

// clusterCtlVerbs are the cluster commands that orchestrate - no node to
// reach first; the others are one node's primitives (runCluster).
var clusterCtlVerbs = map[string]bool{
	"create": true, "apply": true, "check": true, "add": true, "remove": true, "replace": true, "delete": true,
	"vars": true, "certificate": true, "tickets": true, "list": true, "journal": true, "checkout": true,
}

// clusterCtl is the orchestrator for one fleet context.
type clusterCtl struct {
	*clusters.Engine
	ctxName string
	c       *cliContext
	cert    tls.Certificate

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

func openClusterCtl(globalCtx string) *clusterCtl {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, c := currentContext(cfg, globalCtx)
	if c == nil || c.Fleet == nil {
		log.Fatalf("janusctl cluster: %q isn't a fleet context - a cluster without a Controller is run from its fleet's (janusctl fleet init); a Controller's are on its Clusters page", name)
	}
	if c.Fleet.Pending {
		log.Fatalf("janusctl cluster: context %q waits for its issuing CA: janusctl fleet issuer accept GRANT", name)
	}
	if c.Role != pki.RoleAdmin {
		log.Fatalf("janusctl cluster: context %q signs as %s - a cluster's orchestrator acts as %s", name, c.Role, pki.RoleAdmin)
	}
	cert, err := fleetClientCert(name, c, nil)
	if err != nil {
		log.Fatal(err)
	}
	_ = cfg.save()
	store, err := clusters.Open(contextDir(name), nil)
	if err != nil {
		log.Fatal(err)
	}
	o := &clusterCtl{ctxName: name, c: c, cert: cert, conns: map[string]*grpc.ClientConn{}}
	o.Engine = &clusters.Engine{
		Store:     store,
		Nodes:     clusters.GRPC{Conn: o.conn, ConfirmFirewall: o.confirmFirewall},
		Ready:     o.ready,
		ManagedBy: "janusctl fleet " + c.Fleet.Name,
		Advisory:  true,
	}
	return o
}

func (o *clusterCtl) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, c := range o.conns {
		c.Close()
	}
}

// node is the context's node named name.
func (o *clusterCtl) node(name string) (ctxNode, error) {
	for _, n := range o.c.Nodes {
		if n.Name == name {
			return n, nil
		}
	}
	return ctxNode{}, fmt.Errorf("no node %q in context %q (janusctl fleet adopt)", name, o.ctxName)
}

func (o *clusterCtl) dial(m clusters.Member) (*grpc.ClientConn, error) {
	n, err := o.node(m.NodeID)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(n.CAPEM)) {
		return nil, fmt.Errorf("%s: no CA in the context", n.Name)
	}
	return grpc.NewClient(n.Address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{o.cert}, RootCAs: pool, MinVersion: tls.VersionTLS13})))
}

// conn is member m's connection, made once.
func (o *clusterCtl) conn(_ context.Context, m clusters.Member) (grpc.ClientConnInterface, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.conns[m.Name]; ok {
		return c, nil
	}
	c, err := o.dial(m)
	if err != nil {
		return nil, err
	}
	o.conns[m.Name] = c
	return c, nil
}

// confirmFirewall confirms m's ruleset on trial over fresh connections,
// until shortly before it reverts: one that shuts this machine out can't
// be, and reverts by itself.
func (o *clusterCtl) confirmFirewall(_ context.Context, m clusters.Member, revertAt time.Time) error {
	var last error
	for time.Until(revertAt) > 2*time.Second {
		if last = o.confirmFresh(m); last == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("couldn't reach the node over a new connection - the ruleset reverts by itself: %v", last)
}

func (o *clusterCtl) confirmFresh(m clusters.Member) error {
	conn, err := o.dial(m)
	if err != nil {
		return err
	}
	defer conn.Close()
	c, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err = janusv1alpha1.NewNetworkServiceClient(conn).FirewallConfirm(c, &emptypb.Empty{}, grpc.WaitForReady(true))
	return err
}

// ready: m's node is one of the context's.
func (o *clusterCtl) ready(_ context.Context, m clusters.Member) error {
	_, err := o.node(m.NodeID)
	return err
}

// cluster is the context's cluster named name.
func (o *clusterCtl) cluster(name string) *clusters.Cluster {
	for _, c := range o.Store.List() {
		if c.Name == name {
			return c
		}
	}
	log.Fatalf("janusctl cluster: no cluster %q in context %q (janusctl cluster list)", name, o.ctxName)
	return nil
}

// forCluster is a context for cluster id's calls: its id lets them
// through the members' advisory locks.
func forCluster(id string, timeout time.Duration) (context.Context, context.CancelFunc) {
	c, cancel := context.WithTimeout(context.Background(), timeout)
	return metadata.AppendToOutgoingContext(c, pki.ClusterApplyKey, id), cancel
}

// exclusive holds cluster id's lock file while an operation runs: one at
// a time from this machine.
func (o *clusterCtl) exclusive(id string) func() {
	dir := filepath.Join(contextDir(o.ctxName), "clusters", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatalf("janusctl cluster: another janusctl works on this cluster: wait for it")
	}
	return func() { f.Close() }
}

// report prints what cluster id's journal got since since, oldest first.
func (o *clusterCtl) report(id string, since time.Time) {
	entries := o.Store.Entries(id, 200)
	slices.Reverse(entries)
	for _, e := range entries {
		if e.Time.Before(since) {
			continue
		}
		line := e.Message
		if e.Error != "" {
			line += ": " + e.Error
		}
		fmt.Println("  " + line)
	}
}

// member is the member a node of the context makes: its name, the
// address the others reach it on - the context's, unless address says.
func (o *clusterCtl) member(node, address string) clusters.Member {
	n, err := o.node(node)
	if err != nil {
		log.Fatal(err)
	}
	if address == "" {
		host, _, err := net.SplitHostPort(n.Address)
		if ip := net.ParseIP(host); err != nil || ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			log.Fatalf("janusctl cluster: %s's address in the context is %s - not where the other members reach it: give that IP (-address %s=IP)", n.Name, n.Address, n.Name)
		}
		address = host
	}
	return clusters.Member{NodeID: n.Name, Name: n.Name, Address: address}
}

// addresses parses -address NODE=IP flags.
type addresses map[string]string

func (a addresses) String() string { return "" }
func (a addresses) Set(v string) error {
	node, ip, ok := strings.Cut(v, "=")
	if !ok || net.ParseIP(ip) == nil {
		return fmt.Errorf("%q: NODE=IP", v)
	}
	a[node] = ip
	return nil
}

// memberVars parses -var KEY=VALUE flags.
type memberVars map[string]string

func (v memberVars) String() string { return "" }
func (v memberVars) Set(s string) error {
	k, val, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("%q: KEY=VALUE", s)
	}
	v[k] = val
	return nil
}

// readConfigDir is a cluster's configuration in dir: haproxy.cfg,
// files/..., keepalived.conf, bird.conf, firewall.nft.
func readConfigDir(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || (rel != "files" && !strings.HasPrefix(rel, "files/"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := clusters.CheckFiles(files); err != nil {
		return nil, fmt.Errorf("%s: %w - a file holding a key goes to the members with janusctl cluster certificate", dir, err)
	}
	return files, nil
}

// lineChanges is how many lines b adds to a and takes away (an LCS).
func lineChanges(a, b []byte) (added, removed int) {
	x, y := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	if len(a) == 0 {
		x = nil
	}
	if len(b) == 0 {
		y = nil
	}
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	common := lcs[0][0]
	return len(y) - common, len(x) - common
}

func clusterCtlUsage(verb string) {
	printHelp(os.Stderr, commands.sub("cluster").sub(verb), []string{"cluster", verb})
	os.Exit(2)
}

func runClusterCtl(globalCtx string, args []string) {
	actor := func(o *clusterCtl) string { return o.c.User }
	switch args[0] {
	case "list":
		o := openClusterCtl(globalCtx)
		defer o.close()
		tw := table("CLUSTER", "ID", "MEMBERS", "VERSION", "MANAGES")
		for _, c := range o.Store.List() {
			var names []string
			for _, m := range c.Members {
				names = append(names, m.Name)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", c.Name, clusters.ShortID(c.ID), strings.Join(names, ","), c.Desired, strings.Join(c.Managed, ","))
		}
		_ = tw.Flush()
	case "create":
		fs := flag.NewFlagSet("cluster create", flag.ExitOnError)
		source := fs.String("source", "", "the node whose configuration becomes version 1, its certificates and keys every member's")
		port := fs.Int("peers-port", clusters.DefaultPeersPort, "HAProxy's peers port")
		dir := fs.String("dir", "", "write version 1's files there, to edit and apply")
		addr := addresses{}
		fs.Var(addr, "address", "NODE=IP: the address the other members reach NODE on (default: the context's)")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 2 || *source == "" {
			clusterCtlUsage("create")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		name, nodes := fs.Arg(0), fs.Args()[1:]
		if !slices.Contains(nodes, *source) {
			log.Fatalf("janusctl cluster create: the source %s is one of the nodes", *source)
		}
		var members []clusters.Member
		for _, n := range nodes {
			members = append(members, o.member(n, addr[n]))
		}
		c, err := o.Store.Create(name, *port, []string{"haproxy", "files"})
		check("cluster create", err)
		c, err = o.Store.Update(c.ID, func(c *clusters.Cluster) error {
			c.Members, c.SecretsFrom = members, *source
			return nil
		})
		check("cluster create", err)
		defer o.exclusive(c.ID)()
		since := time.Now()
		o.Journal(c.ID, actor(o), "create", "", 0, "cluster %s created with %d members, from %s", name, len(members), *source)
		fmt.Printf("Cluster %s (%s): checking %s, then joining them\n", name, clusters.ShortID(c.ID), strings.Join(nodes, ", "))
		ctx, cancel := forCluster(c.ID, 10*time.Minute)
		defer cancel()
		if err := o.Create(ctx, c.ID, *source, actor(o)); err != nil {
			o.report(c.ID, since)
			// Back as it was: the members leave, the record goes.
			_ = o.Remove(ctx, c.ID, true)
			log.Fatalf("janusctl cluster create: %v - nothing kept; the members left it", err)
		}
		o.report(c.ID, since)
		if *dir != "" {
			writeVersion(o, c.ID, 1, *dir)
		}
		fmt.Printf("Cluster %s: %d members, version 1 from %s on each. Its configuration: janusctl cluster checkout %s DIR, edit, janusctl cluster apply %s DIR.\n", name, len(members), *source, name, name)
	case "checkout":
		fs := flag.NewFlagSet("cluster checkout", flag.ExitOnError)
		version := fs.Int("version", 0, "this version (default: the one the members run)")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 2 {
			clusterCtlUsage("checkout")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		n := *version
		if n == 0 {
			n = c.Desired
		}
		writeVersion(o, c.ID, n, fs.Arg(1))
	case "apply":
		fs := flag.NewFlagSet("cluster apply", flag.ExitOnError)
		manage := fs.String("manage", "", "what the cluster manages from now on: haproxy,files,vrrp,bgp,firewall (default: as before)")
		dryRun := fs.Bool("dry-run", false, "say what would change, change nothing")
		note := fs.String("m", "", "what this version changes")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 2 {
			clusterCtlUsage("apply")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		files, err := readConfigDir(fs.Arg(1))
		check("cluster apply", err)
		managed := c.Managed
		if *manage != "" {
			managed = strings.Split(*manage, ",")
			for _, d := range managed {
				if !slices.Contains(clusters.Domains, d) {
					log.Fatalf("janusctl cluster apply: -manage: unknown domain %q (%s)", d, strings.Join(clusters.Domains, ", "))
				}
			}
		}
		var current map[string][]byte
		if v, err := o.Store.Version(c.ID, c.Desired); err == nil {
			current = v.Content
		}
		changed := printChanges(current, files)
		if !slices.Equal(managed, c.Managed) {
			fmt.Printf("  manages %s (was %s)\n", strings.Join(managed, ","), strings.Join(c.Managed, ","))
		}
		stale := false
		st := o.Store.State(c.ID)
		for _, m := range c.Members {
			if st.Members[m.Name].Applied != c.Desired {
				stale = true
			}
		}
		if *dryRun {
			return
		}
		if !changed && slices.Equal(managed, c.Managed) && !stale {
			fmt.Printf("Cluster %s: every member runs version %d already - nothing to apply\n", c.Name, c.Desired)
			return
		}
		defer o.exclusive(c.ID)()
		since := time.Now()
		n := c.Desired
		if changed {
			v, err := o.Store.NewVersion(c.ID, actor(o), *note, files)
			check("cluster apply", err)
			n = v.Version
			o.Journal(c.ID, actor(o), "version", "", n, "version %d made: %s", n, clusters.ListOrNone([]string{*note}))
		}
		if !slices.Equal(managed, c.Managed) {
			_, err := o.Store.Update(c.ID, func(c *clusters.Cluster) error {
				c.Managed = managed
				return c.Check()
			})
			check("cluster apply", err)
		}
		ctx, cancel := forCluster(c.ID, 10*time.Minute)
		defer cancel()
		err = o.Apply(ctx, c.ID, n, nil, actor(o))
		o.report(c.ID, since)
		check("cluster apply", err)
		fmt.Printf("Cluster %s: version %d on every member\n", c.Name, n)
	case "check":
		fs := flag.NewFlagSet("cluster check", flag.ExitOnError)
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 1 {
			clusterCtlUsage("check")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		defer o.exclusive(c.ID)()
		since := time.Now()
		ctx, cancel := forCluster(c.ID, 5*time.Minute)
		defer cancel()
		o.Reconcile(ctx, c)
		o.report(c.ID, since)
		if !printClusterCheck(c, o.Store.State(c.ID)) {
			os.Exit(1)
		}
	case "add":
		fs := flag.NewFlagSet("cluster add", flag.ExitOnError)
		address := fs.String("address", "", "the address the other members reach the node on (default: the context's)")
		vars := memberVars{}
		fs.Var(vars, "var", "KEY=VALUE: a variable the cluster's templates use for it, repeatable")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 2 {
			clusterCtlUsage("add")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		defer o.exclusive(c.ID)()
		since := time.Now()
		ctx, cancel := forCluster(c.ID, 10*time.Minute)
		defer cancel()
		m := o.member(fs.Arg(1), *address)
		if len(vars) > 0 {
			m.Vars = vars
		}
		err := o.AddMember(ctx, c.ID, m, actor(o))
		o.report(c.ID, since)
		check("cluster add", err)
	case "remove":
		fs := flag.NewFlagSet("cluster remove", flag.ExitOnError)
		force := fs.Bool("force", false, "even if it doesn't drain or answer - a node gone for good")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 2 {
			clusterCtlUsage("remove")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		defer o.exclusive(c.ID)()
		since := time.Now()
		ctx, cancel := forCluster(c.ID, 10*time.Minute)
		defer cancel()
		err := o.RemoveMember(ctx, c.ID, fs.Arg(1), *force, actor(o))
		o.report(c.ID, since)
		check("cluster remove", err)
	case "replace":
		fs := flag.NewFlagSet("cluster replace", flag.ExitOnError)
		address := fs.String("address", "", "the address the other members reach the new node on (default: the context's)")
		vars := memberVars{}
		fs.Var(vars, "var", "KEY=VALUE: a variable the cluster's templates use for it, repeatable (default: the old member's)")
		newImage := fs.Bool("new-image", false, "the new node may run another image - the cluster changing image member by member")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 3 {
			clusterCtlUsage("replace")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		old, node := fs.Arg(1), fs.Arg(2)
		m := o.member(node, *address)
		if len(vars) > 0 {
			m.Vars = vars
		}
		defer o.exclusive(c.ID)()
		since := time.Now()
		ctx, cancel := forCluster(c.ID, 20*time.Minute)
		defer cancel()
		fmt.Printf("Cluster %s: %s joins drained, learns its stick tables, then takes %s's place\n", c.Name, node, old)
		err := o.Replace(ctx, c.ID, old, m, clusters.ReplaceOptions{NewImage: *newImage}, actor(o))
		o.report(c.ID, since)
		check("cluster replace", err)
		fmt.Printf("%s replaced %s. %s is on its own, still drained until it reboots (janusctl -n %s cluster undrain to use it again)\n", node, old, old, old)
	case "delete":
		fs := flag.NewFlagSet("cluster delete", flag.ExitOnError)
		force := fs.Bool("force", false, "even if a member doesn't answer")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 1 {
			clusterCtlUsage("delete")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		release := o.exclusive(c.ID)
		ctx, cancel := forCluster(c.ID, 5*time.Minute)
		defer cancel()
		err := o.Remove(ctx, c.ID, *force)
		release()
		check("cluster delete", err)
		fmt.Printf("Cluster %s deleted: its members are on their own again, its CA gone\n", c.Name)
	case "vars":
		fs := flag.NewFlagSet("cluster vars", flag.ExitOnError)
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 2 {
			clusterCtlUsage("vars")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		member := fs.Arg(1)
		if c.Member(member) == nil {
			log.Fatalf("janusctl cluster vars: %s isn't a member of %s", member, c.Name)
		}
		if fs.NArg() > 2 {
			c2, err := o.Store.Update(c.ID, func(c *clusters.Cluster) error {
				m := c.Member(member)
				if m.Vars == nil {
					m.Vars = map[string]string{}
				}
				for _, kv := range fs.Args()[2:] {
					k, v, ok := strings.Cut(kv, "=")
					if !ok || k == "" {
						return fmt.Errorf("%q: KEY=VALUE (KEY= unsets it)", kv)
					}
					if v == "" {
						delete(m.Vars, k)
					} else {
						m.Vars[k] = v
					}
				}
				return c.Check()
			})
			check("cluster vars", err)
			c = c2
		}
		vars := c.Member(member).Vars
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, vars[k])
		}
		if fs.NArg() > 2 {
			fmt.Fprintf(os.Stderr, "The next apply renders %s's templates with them: janusctl cluster apply %s DIR\n", member, c.Name)
		}
	case "certificate":
		fs := flag.NewFlagSet("cluster certificate", flag.ExitOnError)
		name := fs.String("name", "", "its name in HAProxy's store (default: the file's)")
		crtList := fs.String("crt-list", "", "a crt-list to bind it into")
		sni := fs.String("sni", "", "the names it's served for in that crt-list, comma-separated")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 2 {
			clusterCtlUsage("certificate")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		pemBundle, err := os.ReadFile(fs.Arg(1))
		check("cluster certificate", err)
		if *name == "" {
			*name = filepath.Base(fs.Arg(1))
		}
		req := &janusv1alpha1.CertificateUploadRequest{Name: *name, PemBundle: pemBundle, CrtList: *crtList}
		if *sni != "" {
			req.Sni = strings.Split(*sni, ",")
		}
		defer o.exclusive(c.ID)()
		since := time.Now()
		ctx, cancel := forCluster(c.ID, 5*time.Minute)
		defer cancel()
		err = o.UploadCertificate(ctx, c.ID, req, actor(o))
		o.report(c.ID, since)
		check("cluster certificate", err)
	case "tickets":
		fs := flag.NewFlagSet("cluster tickets", flag.ExitOnError)
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 1 {
			clusterCtlUsage("tickets")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		defer o.exclusive(c.ID)()
		since := time.Now()
		ctx, cancel := forCluster(c.ID, 5*time.Minute)
		defer cancel()
		err := o.RotateTickets(ctx, c)
		if err == nil {
			_ = o.Store.UpdateState(c.ID, func(s *clusters.State) { s.TicketsRotated = time.Now().UTC() })
		}
		o.report(c.ID, since)
		check("cluster tickets", err)
	case "journal":
		fs := flag.NewFlagSet("cluster journal", flag.ExitOnError)
		n := fs.Int("n", 50, "the newest N entries")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 1 {
			clusterCtlUsage("journal")
		}
		o := openClusterCtl(globalCtx)
		defer o.close()
		c := o.cluster(fs.Arg(0))
		entries := o.Store.Entries(c.ID, *n)
		slices.Reverse(entries)
		tw := table("TIME", "WHO", "WHAT", "MEMBER", "VERSION", "")
		for _, e := range entries {
			v := ""
			if e.Version > 0 {
				v = fmt.Sprint(e.Version)
			}
			msg := e.Message
			if e.Error != "" {
				msg += ": " + e.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, e.Member, v, msg)
		}
		_ = tw.Flush()
	}
}

// writeVersion writes version n of cluster id's files into dir - a new
// directory, or one this version's files only replace.
func writeVersion(o *clusterCtl, id string, n int, dir string) {
	v, err := o.Store.Version(id, n)
	check("cluster checkout", err)
	for p, data := range v.Content {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("Version %d's %d file(s) written to %s\n", n, len(v.Content), dir)
}

// printChanges says what files change from before: whether any does.
func printChanges(before, after map[string][]byte) bool {
	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	changed := false
	for _, p := range sorted {
		a, inA := before[p]
		b, inB := after[p]
		switch {
		case !inA:
			add, _ := lineChanges(nil, b)
			fmt.Printf("  %s: added (%d lines)\n", p, add)
		case !inB:
			fmt.Printf("  %s: removed\n", p)
		case string(a) != string(b):
			add, rm := lineChanges(a, b)
			fmt.Printf("  %s: +%d -%d lines\n", p, add, rm)
		default:
			continue
		}
		changed = true
	}
	return changed
}

// printClusterCheck is each member's state: whether it's all well.
func printClusterCheck(c *clusters.Cluster, st clusters.State) bool {
	ok := true
	tw := table("MEMBER", "VERSION", "PEERS", "TABLES", "CERTIFICATE", "STATE")
	for _, m := range c.Members {
		s := st.Members[m.Name]
		var state []string
		if s.Drained {
			state = append(state, "drained")
		}
		if len(s.Drift) > 0 {
			state = append(state, "drifted: "+strings.Join(s.Drift, ","))
		}
		if len(s.Released) > 0 {
			state = append(state, "released: "+strings.Join(s.Released, ","))
		}
		if s.Error != "" {
			state = append(state, "error: "+s.Error)
		}
		if s.Applied != c.Desired || s.PeersUp < s.PeersTotal || len(state) > 0 {
			ok = false
		}
		if len(state) == 0 {
			state = append(state, "ok")
		}
		tables := "in sync"
		if !s.TablesSynced {
			tables = "learning"
		}
		cert := "-"
		if !s.CertNotAfter.IsZero() {
			cert = s.CertNotAfter.Local().Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d/%d\t%s\t%s\t%s\n", m.Name, s.Applied, s.PeersUp, s.PeersTotal, tables, cert, strings.Join(state, "; "))
	}
	_ = tw.Flush()
	if !ok {
		fmt.Fprintf(os.Stderr, "Cluster %s wants a look - version %d is the cluster's; a drifted member gets it back with janusctl cluster apply\n", c.Name, c.Desired)
	}
	return ok
}
