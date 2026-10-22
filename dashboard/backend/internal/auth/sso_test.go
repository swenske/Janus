package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const week = 7 * 24 * time.Hour

// ssoStore is a store set up with the local admin root (no second factor
// required), its clock at now.
func ssoStore(t *testing.T) (*Store, *time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	s := open(t, dir)
	s.SetSealer(xorSealer{})
	now := time.Unix(1_800_000_000, 0).UTC()
	s.now = func() time.Time { return now }
	if _, err := s.Setup("root", "long-enough", MFANobody); err != nil {
		t.Fatal(err)
	}
	return s, &now, dir
}

func kcIdentity(sub, name string) Identity {
	return Identity{Provider: "kc", Issuer: "https://kc.example.com/realms/janus", Subject: sub, Username: name}
}

var (
	asAdmin    = SSOAccess{Role: Admin, Matched: true}
	asReader   = SSOAccess{Role: Reader, Matched: true}
	asNobody   = SSOAccess{}
	webGrant   = Grant{Role: Operator, Selector: map[string]string{"site": "web"}}
	asWebGrant = SSOAccess{Grants: []Grant{webGrant}, Matched: true}
)

func TestSSOSignInMakesAnAccount(t *testing.T) {
	s, now, _ := ssoStore(t)
	alice := kcIdentity("uuid-alice", "alice")
	u, err := s.SSOSignIn(alice, []string{"/janus-admins"}, asAdmin, week)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "alice" || u.Role != Admin || u.HasPassword || u.SSO == nil || u.SSO.Provider != "kc" || len(u.Identities) != 1 {
		t.Fatalf("account: %+v", u)
	}
	if !u.SSO.ValidUntil.Equal(now.Add(week)) || !slices.Equal(u.SSO.Groups, []string{"/janus-admins"}) || u.LastLoginAt == nil {
		t.Fatalf("sso: %+v", u.SSO)
	}
	if _, err := s.Authenticate("alice", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an account without a password signed in with none: %v", err)
	}
	// Again: the same account, by its identity - even under another name.
	alice.Username = "alice.renamed"
	if u, err = s.SSOSignIn(alice, []string{"/janus-admins"}, asAdmin, week); err != nil || u.Name != "alice" || u.Identities[0].Username != "alice.renamed" {
		t.Fatalf("second sign-in: %+v %v", u, err)
	}
	if len(s.Users()) != 2 {
		t.Fatalf("%d accounts", len(s.Users()))
	}
	// The same name from another subject (or issuer) is another person.
	if _, err := s.SSOSignIn(kcIdentity("uuid-mallory", "alice"), nil, asAdmin, week); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a name taken: %v", err)
	}
	other := kcIdentity("uuid-alice", "alice")
	other.Issuer = "https://evil.example.com"
	if _, err := s.SSOSignIn(other, nil, asAdmin, week); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("the same subject from another issuer: %v", err)
	}
	// A local account's name is never taken over.
	if _, err := s.SSOSignIn(kcIdentity("uuid-root", "root"), nil, asAdmin, week); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("the local root: %v", err)
	}
	if _, err := s.SSOSignIn(kcIdentity("uuid-x", "Not A Name!"), nil, asAdmin, week); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("an invalid name: %v", err)
	}
	if _, err := s.SSOSignIn(kcIdentity("uuid-carol", "carol"), nil, asNobody, week); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("no rule matched: %v", err)
	}
	if _, ok := s.User("carol"); ok {
		t.Fatal("an account made for someone without access")
	}
	// Disabled: refused.
	if _, err := s.UpdateUser("alice", Change{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SSOSignIn(alice, nil, asAdmin, week); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
}

func TestSSOSignInBeforeSetup(t *testing.T) {
	s := open(t, t.TempDir())
	if _, err := s.SSOSignIn(kcIdentity("u", "alice"), nil, asAdmin, week); err == nil {
		t.Fatal("an account made before the setup")
	}
	if !s.SetupRequired() {
		t.Fatal("the setup was taken")
	}
}

func TestSSOAccessFollowsTheRules(t *testing.T) {
	s, _, _ := ssoStore(t)
	alice := kcIdentity("uuid-alice", "alice")
	if _, err := s.SSOSignIn(alice, []string{"/janus-admins"}, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.NewSSOSession("alice", SSOSession{Provider: "kc"})
	// A step up keeps the sessions.
	up := SSOAccess{Role: Admin, Grants: []Grant{webGrant}, Matched: true}
	if _, err := s.SSOSignIn(alice, nil, up, week); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(tok, true); !ok {
		t.Fatal("a session ended by a step up")
	}
	// A step down ends them.
	if u, err := s.SSOSignIn(alice, []string{"/janus-readers"}, asReader, week); err != nil || u.Role != Reader || len(u.Grants) != 0 {
		t.Fatalf("demoted: %+v %v", u, err)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Fatal("a session survived a step down")
	}
	// No rule matching any more: no role, no grant, refused.
	tok, _ = s.NewSSOSession("alice", SSOSession{Provider: "kc"})
	if _, err := s.SSOSignIn(alice, nil, asNobody, week); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("no access: %v", err)
	}
	if u, _ := s.User("alice"); u.Role != None || len(u.Grants) != 0 {
		t.Fatalf("kept its access: %+v", u)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Fatal("a session survived losing every access")
	}
	// Grants only.
	if u, err := s.SSOSignIn(alice, []string{"/web"}, asWebGrant, week); err != nil || u.Role != None || !slices.EqualFunc(u.Grants, []Grant{webGrant}, SameGrant) {
		t.Fatalf("grants: %+v %v", u, err)
	}
}

func TestSSOManagedAccounts(t *testing.T) {
	s, _, _ := ssoStore(t)
	if _, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Change{"role": {Role: ptr(Reader)}, "grants": {Grants: &[]Grant{}}, "password": {Password: ptr("long-enough")}} {
		if _, err := s.UpdateUser("alice", c); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.ChangePassword("alice", "", "long-enough"); !errors.Is(err, ErrManaged) {
		t.Errorf("own password: %v", err)
	}
	if err := s.UnlinkIdentity("alice", s.users["alice"].Identities[0].ID); !errors.Is(err, ErrManaged) {
		t.Errorf("unlink: %v", err)
	}
	// The last admin with a password stays one, SSO admins or not.
	if _, err := s.UpdateUser("root", Change{Role: ptr(Reader)}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote root: %v", err)
	}
	if _, err := s.UpdateUser("root", Change{Disabled: ptr(true)}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disable root: %v", err)
	}
	if err := s.DeleteUser("root"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("delete root: %v", err)
	}
	if _, err := s.UpdateUser("alice", Change{Disabled: ptr(true)}); err != nil {
		t.Errorf("disable an SSO admin: %v", err)
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Errorf("delete an SSO admin: %v", err)
	}
}

func TestSSOLapse(t *testing.T) {
	s, now, _ := ssoStore(t)
	alice := kcIdentity("uuid-alice", "alice")
	if _, err := s.SSOSignIn(alice, nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.AddSSHKey("alice", "laptop", authorized(t, edPub), "", 0); err != nil {
		t.Fatal(err)
	}
	fp := s.users["alice"].SSHKeys[0].Fingerprint
	tok, _ := s.NewSSOSession("alice", SSOSession{Provider: "kc"})
	*now = now.Add(week)
	if u, _ := s.User("alice"); !u.Lapsed(*now) {
		t.Fatal("not lapsed after a week")
	}
	if _, _, ok := s.Session(tok, false); ok {
		t.Fatal("a lapsed account's session lives")
	}
	if _, _, _, err := s.SSHKeyFor("alice", fp); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a lapsed account's SSH key: %v", err)
	}
	// Signing in through the provider again revives it.
	if _, err := s.SSOSignIn(alice, nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SSHKeyFor("alice", fp); err != nil {
		t.Fatalf("SSH key after signing in again: %v", err)
	}
	// Never lapsing.
	if _, err := s.SSOSignIn(alice, nil, asAdmin, 0); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(100 * week)
	if u, _ := s.User("alice"); u.Lapsed(*now) || u.SSO.ValidUntil != nil {
		t.Fatal("lapsed without a limit")
	}
	if err := s.LapseProvider("kc"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.User("alice"); !u.Lapsed(*now) {
		t.Fatal("its provider gone, not lapsed")
	}
}

func TestResyncSSO(t *testing.T) {
	s, now, _ := ssoStore(t)
	for _, who := range []struct{ sub, name, group string }{{"u1", "alice", "/janus-admins"}, {"u2", "bob", "/janus-readers"}} {
		if _, err := s.SSOSignIn(kcIdentity(who.sub, who.name), []string{who.group}, asReader, week); err != nil {
			t.Fatal(err)
		}
	}
	other := kcIdentity("u3", "carol")
	other.Provider = "nas"
	if _, err := s.SSOSignIn(other, []string{"/janus-admins"}, asReader, week); err != nil {
		t.Fatal(err)
	}
	bobs, _ := s.NewSSOSession("bob", SSOSession{Provider: "kc"})
	rules := func(groups []string) SSOAccess {
		if slices.Contains(groups, "/janus-admins") {
			return asAdmin
		}
		return asNobody
	}
	n, err := s.ResyncSSO("kc", rules, 2*week)
	if err != nil || n != 2 {
		t.Fatalf("resync: %d, %v", n, err)
	}
	if u, _ := s.User("alice"); u.Role != Admin || !u.SSO.ValidUntil.Equal(now.Add(2*week)) {
		t.Fatalf("alice: %+v", u)
	}
	if u, _ := s.User("bob"); u.Role != None {
		t.Fatalf("bob: %+v", u)
	}
	if _, _, ok := s.Session(bobs, true); ok {
		t.Fatal("bob's session survived losing his access")
	}
	if u, _ := s.User("carol"); u.Role != Reader {
		t.Fatalf("another provider's account changed: %+v", u)
	}
	if n, _ := s.ResyncSSO("kc", rules, 2*week); n != 0 {
		t.Fatalf("nothing to change, %d changed", n)
	}
}

func TestLinkIdentity(t *testing.T) {
	s, _, _ := ssoStore(t)
	if _, err := s.CreateUser("olga", Operator, "long-enough", nil); err != nil {
		t.Fatal(err)
	}
	olga := kcIdentity("uuid-olga", "olga.kc")
	id, err := s.LinkIdentity("olga", olga)
	if err != nil {
		t.Fatal(err)
	}
	// Signing in through the provider: olga, her own role kept.
	u, err := s.SSOSignIn(olga, []string{"/janus-readers"}, asReader, week)
	if err != nil || u.Name != "olga" || u.Role != Operator || u.SSO != nil || !u.HasPassword {
		t.Fatalf("linked sign-in: %+v %v", u, err)
	}
	// ... only while a rule lets her in - her account untouched.
	if _, err := s.SSOSignIn(olga, nil, asNobody, week); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("no access: %v", err)
	}
	if u, _ := s.User("olga"); u.Role != Operator {
		t.Fatalf("a refused sign-in changed her role: %+v", u)
	}
	if _, err := s.LinkIdentity("root", olga); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("an identity linked twice: %v", err)
	}
	if _, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkIdentity("alice", kcIdentity("uuid-other", "x")); !errors.Is(err, ErrManaged) {
		t.Fatalf("linking to an account a provider made: %v", err)
	}
	for i := 1; i < MaxIdentities; i++ {
		if _, err := s.LinkIdentity("olga", kcIdentity("more-"+string(rune('a'+i)), "x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.LinkIdentity("olga", kcIdentity("one-too-many", "x")); err == nil {
		t.Fatal("more identities than MaxIdentities")
	}
	if err := s.UnlinkIdentity("olga", id.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SSOSignIn(olga, nil, asReader, week); err != nil {
		// Unlinked, the identity is a stranger again: an account of its own.
		t.Fatalf("after unlinking: %v", err)
	}
	if _, ok := s.User("olga.kc"); !ok {
		t.Fatal("the unlinked identity didn't get its own account")
	}
}

func TestSSOSessions(t *testing.T) {
	s, now, _ := ssoStore(t)
	if err := s.SetSettings(Settings{SessionIdleMinutes: 30, SessionMaxHours: 12, MFARequired: MFAEveryone}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("alice")
	// The provider's second factor counts: nothing to set up.
	tok, _ := s.NewSSOSession("alice", SSOSession{Provider: "kc", IDToken: "idt", ProviderMFA: true})
	_, ss, _ := s.Session(tok, true)
	if got := s.Needs(u, ss); len(got) != 0 || !ss.MFA || ss.Trusted || ss.Provider != "kc" || ss.IDToken != "idt" {
		t.Fatalf("provider MFA: needs %v, %+v", got, ss)
	}
	// ... yet it doesn't stand for the Controller's.
	if _, _, err := s.TrustBrowser(tok, "Firefox", "10.0.0.1"); err == nil {
		t.Fatal("a browser trusted for a factor the Controller never saw")
	}
	// Otherwise the Controller's policy applies.
	tok2, _ := s.NewSSOSession("alice", SSOSession{Provider: "kc"})
	_, ss, _ = s.Session(tok2, true)
	if got := s.Needs(u, ss); len(got) != 1 || got[0] != "mfa_enroll" {
		t.Fatalf("janus MFA: needs %v", got)
	}
	// A renewed session keeps its provider.
	renewed, err := s.RenewSession(tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Fatal("the old session lives on")
	}
	if _, ss, _ = s.Session(renewed, true); ss.Provider != "kc" || !ss.ProviderMFA || ss.IDToken != "idt" {
		t.Fatalf("renewed: %+v", ss)
	}
	s.EndProviderSessions("kc")
	if _, _, ok := s.Session(renewed, true); ok {
		t.Fatal("a session outlived its provider")
	}
	_ = now
}

func TestProofWithoutPassword(t *testing.T) {
	s, now, _ := ssoStore(t)
	if _, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.NewSSOSession("alice", SSOSession{Provider: "kc"})
	secret, _ := s.StartTOTP(tok)
	code, _ := TOTPCode(secret, *now)
	if _, err := s.EnableTOTP(tok, code); err != nil {
		t.Fatal(err)
	}
	// The factor just set up confirms, five minutes long.
	*now = now.Add(freshFactor)
	if _, err := s.NewRecoveryCodes("alice", Proof{Session: tok}); !errors.Is(err, ErrProofNeeded) {
		t.Fatalf("a factor given too long ago: %v", err)
	}
	if _, err := s.NewRecoveryCodes("alice", Proof{Password: "anything"}); !errors.Is(err, ErrProofNeeded) {
		t.Fatalf("a password for an account without one: %v", err)
	}
	if _, err := s.NewRecoveryCodes("alice", Proof{Code: "000000"}); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("a wrong code: %v", err)
	}
	code, _ = TOTPCode(secret, *now)
	codes, err := s.NewRecoveryCodes("alice", Proof{Code: code})
	if err != nil {
		t.Fatalf("a code: %v", err)
	}
	if _, err := s.NewRecoveryCodes("alice", Proof{Code: code}); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("a code used twice: %v", err)
	}
	if err := s.RemoveTOTP("alice", Proof{Code: codes[0]}); err != nil {
		t.Fatalf("a recovery code: %v", err)
	}
	// A password account still confirms with its password.
	if _, err := s.NewRecoveryCodes("root", Proof{Code: code}); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("root without its password: %v", err)
	}
}

func TestSSOResetFromHost(t *testing.T) {
	s, _, dir := ssoStore(t)
	if _, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), nil, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	if _, err := ResetFromHost(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("alice") // read again: changed from outside
	if u.SSO != nil || !u.HasPassword || len(u.Identities) != 1 || u.Role != Admin {
		t.Fatalf("reset: %+v", u)
	}
	// Its identity is linked now: signing in through the provider, alice.
	if got, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), nil, asReader, week); err != nil || got.Name != "alice" || got.Role != Admin {
		t.Fatalf("after the reset: %+v %v", got, err)
	}
}

func TestSSOReadBack(t *testing.T) {
	s, _, dir := ssoStore(t)
	if _, err := s.SSOSignIn(kcIdentity("uuid-alice", "alice"), []string{"/a"}, asAdmin, week); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, usersFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"subject": "uuid-alice"`) || strings.Contains(string(raw), "has_password") {
		t.Fatalf("users.json: %s", raw)
	}
	s2 := open(t, dir)
	if u, _ := s2.User("alice"); u.SSO == nil || len(u.Identities) != 1 || u.HasPassword {
		t.Fatalf("read back: %+v", u)
	}
	if u, err := s2.SSOSignIn(kcIdentity("uuid-alice", "x"), nil, asAdmin, week); err != nil || u.Name != "alice" {
		t.Fatalf("bound after reading back: %+v %v", u, err)
	}
}
