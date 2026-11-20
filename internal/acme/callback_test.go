package acme

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// callbackHAProxy is janusd's HAProxy manager as janusd wires it to this
// one (haproxyMgr.Env = acmeMgr.Env): its Validate calls Env, and so does
// every start or reload, under the lock its StartedAt takes too
// (internal/haproxy's startOrReload, env()). fakeHAProxy never called
// back, and #694's deadlock went unseen.
type callbackHAProxy struct {
	*fakeHAProxy
	m    *Manager
	lock sync.Mutex // internal/haproxy's Manager.mu
}

func (c *callbackHAProxy) Validate(cfg []byte) (bool, []string) {
	c.m.Env()
	return c.fakeHAProxy.Validate(cfg)
}

func (c *callbackHAProxy) Reload() error {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.m.Env()
	return c.fakeHAProxy.Reload()
}

func (c *callbackHAProxy) StartedAt() time.Time {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.fakeHAProxy.StartedAt()
}

// setupCallback is setup with HAProxy calling back, running.
func setupCallback(t *testing.T) (*Manager, *fakeHAProxy) {
	t.Helper()
	m, fake := setup(t)
	m.opts.HAProxy = &callbackHAProxy{fakeHAProxy: fake, m: m}
	fake.started = time.Now()
	return m, fake
}

// within fails the test when fn hasn't returned after 10 s: a deadlock
// fails it rather than hanging the run (fn's goroutine is left behind).
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: still blocked after 10 s - a deadlock", what)
	}
}

// checkEnv makes sure Env, which takes no lock, says what the manager
// answers HTTP-01 with.
func checkEnv(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	want := m.answerThumbLocked()
	m.mu.Unlock()
	got := strings.Join(m.Env(), " ")
	if want == "" && got != "" || want != "" && got != ThumbprintEnv+"="+want {
		t.Errorf("Env() = %q, the manager answers with %q", got, want)
	}
}

func usesSite() (bool, []string) {
	if _, err := os.Stat(certPath("site")); err != nil {
		return false, []string{"unable to load " + certPath("site")}
	}
	return true, nil
}

// #694: a configuration leaving a certificate out - checked or applied -
// returns, whether haproxy.cfg still uses the certificate or not, with
// HAProxy's validation calling Env as janusd's does.
func TestRemovalWithHAProxyCallingBack(t *testing.T) {
	m, fake := setupCallback(t)
	if errs, err := m.Apply(testConfig(), "", false); err != nil {
		t.Fatal(errs, err)
	}
	checkEnv(t, m)
	without := testConfig()
	without.Certificates = without.Certificates[1:] // "site" left out

	var errs []string
	var err error
	fake.validate = usesSite
	within(t, "acme check without a certificate haproxy.cfg uses", func() { errs, err = m.Apply(without, "", true) })
	if err == nil || !strings.Contains(strings.Join(errs, " "), "unable to load") {
		t.Fatalf("checked while in use: %v %v", errs, err)
	}
	within(t, "acme apply without a certificate haproxy.cfg uses", func() { errs, err = m.Apply(without, "", false) })
	if err == nil {
		t.Fatal("removal accepted while in use")
	}
	if _, err := os.Stat(certPath("site")); err != nil {
		t.Fatal("the file isn't back after a refused removal")
	}
	if _, err := os.Stat(removingDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s left behind: %v", removingDir(), err)
	}

	fake.validate = nil
	within(t, "acme check without a certificate", func() { errs, err = m.Apply(without, "", true) })
	if err != nil {
		t.Fatal(errs, err)
	}
	if _, err := os.Stat(certPath("site")); err != nil {
		t.Fatal("a check removed the file")
	}
	within(t, "acme apply without a certificate", func() { errs, err = m.Apply(without, "", false) })
	if err != nil {
		t.Fatal(errs, err)
	}
	if _, err := os.Stat(certPath("site")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the removed certificate's file is still there")
	}
	checkEnv(t, m)
}

// The same through a cluster's pull: InstallPulledConfig checks the
// certificates the pulled configuration lacks the same way.
func TestPulledConfigWithHAProxyCallingBack(t *testing.T) {
	m, fake := setupCallback(t)
	if errs, err := m.Apply(testConfig(), "", false); err != nil {
		t.Fatal(errs, err)
	}
	pulled := testConfig()
	pulled.Certificates = pulled.Certificates[1:]
	data, err := Marshal(pulled)
	if err != nil {
		t.Fatal(err)
	}
	fake.validate = usesSite
	within(t, "a pull without a certificate haproxy.cfg uses", func() { err = m.InstallPulledConfig(data) })
	if err != nil {
		t.Fatal(err)
	}
	if c := m.Config().Certificates; len(c) != 2 {
		t.Errorf("certificates after the pull: %d - site, still used, should stay", len(c))
	}
	fake.validate = nil
	within(t, "a pull without a certificate", func() { err = m.InstallPulledConfig(data) })
	if err != nil {
		t.Fatal(err)
	}
	if c := m.Config().Certificates; len(c) != 1 || c[0].Name != "wild" {
		t.Errorf("certificates once unused: %v", c)
	}
}

// HAProxy reloading - Env called under its lock - while configurations
// are applied - StartedAt asking for that lock under the manager's: two
// locks taken in both orders deadlocked as soon as Env took the
// manager's, no certificate removed.
func TestApplyWhileHAProxyReloads(t *testing.T) {
	m, _ := setupCallback(t)
	hp := m.opts.HAProxy
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = hp.Reload()
		}
	}()
	var applyErr error
	within(t, "applying while HAProxy reloads", func() {
		for i := range 30 {
			cfg := testConfig()
			if i%2 == 1 {
				cfg.Account.Email = "other@example.com"
			}
			if errs, err := m.Apply(cfg, "", false); err != nil {
				applyErr = errors.Join(err, errors.New(strings.Join(errs, "; ")))
				return
			}
		}
	})
	close(stop)
	wg.Wait()
	if applyErr != nil {
		t.Fatal(applyErr)
	}
	checkEnv(t, m)
}

// A janusd stopped while a removal was being checked - #694's deadlock,
// or a power cut - left the certificate's file in .acme-removing/: the
// next boot puts it back where the certificate is still configured
// without a real one in place, deletes it otherwise.
func TestBootRecoversCertificatesLeftAside(t *testing.T) {
	m, _ := setup(t)
	if errs, err := m.Apply(testConfig(), "", false); err != nil {
		t.Fatal(errs, err)
	}
	issued := func(name string) []byte {
		t.Helper()
		for _, c := range m.Config().Certificates {
			if c.Name == name {
				chain, key := issueTestCert(c.Domains, c.KeyType)
				data, _, err := checkIssued(chain, key, c.Domains)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
		}
		t.Fatalf("no certificate %s", name)
		return nil
	}
	aside := removingDir()
	if err := os.MkdirAll(aside, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// site: a stand-in in place (written at the boot after the deadlock),
	// its real file aside.
	write(filepath.Join(aside, "site.pem"), issued("site"))
	// wild: obtained again since - the copy aside is stale.
	wild := issued("wild")
	write(certPath("wild"), wild)
	write(filepath.Join(aside, "wild.pem"), issued("wild"))
	// gone: no longer configured.
	write(filepath.Join(aside, "gone.pem"), issued("site"))
	if !readBundle(t, "site").Placeholder {
		t.Fatal("site isn't a stand-in to begin with")
	}

	m2 := New(Options{HAProxy: &fakeHAProxy{loaded: map[string]bool{}}, HAProxyConfig: m.opts.HAProxyConfig, Output: &testLog{t}})
	if err := m2.Boot(); err != nil {
		t.Fatal(err)
	}
	if b := readBundle(t, "site"); b.Placeholder || b.Leaf.Issuer.CommonName != "Fake CA" {
		t.Errorf("site's file not put back: issuer %q", b.Leaf.Issuer.CommonName)
	}
	b, err := parseBundle(wild)
	if err != nil {
		t.Fatal(err)
	}
	if got := serialHex(readBundle(t, "wild").Leaf); got != serialHex(b.Leaf) {
		t.Error("wild's newer file replaced by the stale copy")
	}
	if _, err := os.Stat(certPath("gone")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a certificate no longer configured put back")
	}
	if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still there: %v", aside, err)
	}
}
