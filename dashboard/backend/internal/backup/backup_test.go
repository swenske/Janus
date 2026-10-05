package backup

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"
)

// xorSealer stands in for the master key: reversible, bound to purpose.
type fakeSealer struct{}

func (fakeSealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte(purpose+"|"), p...), nil
}

func (fakeSealer) Open(s []byte, purpose string) ([]byte, error) {
	out, ok := bytes.CutPrefix(s, []byte(purpose+"|"))
	if !ok {
		return nil, errors.New("another purpose")
	}
	return out, nil
}

// TestSealOpen: a backup opens with the kit's identity or an admin's SSH
// key, and only with the Controller's signature intact.
func TestSealOpen(t *testing.T) {
	kitID, _ := age.GenerateX25519Identity()
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(edPub)
	sshRecipient, err := ParseRecipient(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err != nil {
		t.Fatal(err)
	}
	signPub, signPriv, _ := ed25519.GenerateKey(rand.Reader)
	archive := bytes.Repeat([]byte("the Controller's data "), 500)
	file, err := Seal(archive, Manifest{Version: "dev", Created: time.Now(), Nodes: map[string]string{"edge-1": ""}}, []age.Recipient{kitID.Recipient(), sshRecipient}, signPriv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(file, []byte(magic+"\n")) || bytes.Contains(file, []byte("the Controller's data")) {
		t.Fatal("not a sealed backup")
	}
	m, enc, err := Open(file, signPub)
	if err != nil || m.Nodes["edge-1"] != "" || m.Format != "janus-backup-1" {
		t.Fatalf("open: %+v %v", m, err)
	}
	got, err := Decrypt(enc, kitID)
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("the kit's identity: %v", err)
	}
	block, _ := ssh.MarshalPrivateKey(edPriv, "")
	sshID, err := agessh.ParseIdentity(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decrypt(enc, sshID); err != nil || !bytes.Equal(got, archive) {
		t.Errorf("the admin's SSH key: %v", err)
	}
	stranger, _ := age.GenerateX25519Identity()
	if _, err := Decrypt(enc, stranger); err == nil {
		t.Error("decrypted by a stranger")
	}

	// Another Controller's key, a changed byte, a forged manifest.
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := Open(file, otherPub); !errors.Is(err, ErrSignature) {
		t.Errorf("another Controller's key: %v", err)
	}
	tampered := append([]byte{}, file...)
	tampered[len(tampered)-10] ^= 1
	if _, _, err := Open(tampered, signPub); !errors.Is(err, ErrSignature) {
		t.Errorf("a changed byte: %v", err)
	}
	forged, _ := Seal(archive, Manifest{}, []age.Recipient{kitID.Recipient()}, otherPriv)
	if _, _, err := Open(forged, signPub); !errors.Is(err, ErrSignature) {
		t.Errorf("a backup someone else signed: %v", err)
	}
	if _, _, err := Open([]byte("hello"), signPub); err == nil {
		t.Error("not a backup")
	}
}

// TestKitLifecycle: the kit is made, given back to be confirmed - only
// then do backups go; a new kit counts once confirmed, the previous one
// until then.
func TestKitLifecycle(t *testing.T) {
	s, err := OpenStore(t.TempDir(), fakeSealer{}, "ctl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seal([]byte("x"), Manifest{}); err == nil {
		t.Error("sealed without a kit")
	}
	on := DefaultSettings
	on.Enabled, on.Endpoint, on.Bucket, on.AccessKey = true, "https://s3.example", "b", "ak"
	secret := "sk"
	if err := s.SetSettings(on, &secret); err == nil || !strings.Contains(err.Error(), "kit") {
		t.Errorf("enabled without a kit: %v", err)
	}
	pass, err := s.StartKit()
	if err != nil {
		t.Fatal(err)
	}
	kit, _ := s.PendingKit()
	if !strings.HasPrefix(string(kit), "-----BEGIN AGE ENCRYPTED FILE-----") {
		t.Fatalf("kit: %.40q", kit)
	}
	if err := s.ConfirmKit(kit, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"); !errors.Is(err, ErrKit) {
		t.Errorf("a wrong passphrase: %v", err)
	}
	if err := s.ConfirmKit(kit, strings.ToLower(pass)); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.Kit != KitReady || st.KitWaiting {
		t.Errorf("after confirming: %+v", st)
	}
	if err := s.SetSettings(on, &secret); err != nil {
		t.Fatal(err)
	}
	file, err := s.Seal([]byte("archive"), Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	opened, _ := OpenKit(kit, pass)
	id, signer, _ := opened.Keys()
	m, enc, err := Open(file, signer)
	if err != nil || m.Controller != "ctl" {
		t.Fatalf("open with the kit's key: %v", err)
	}
	if got, err := Decrypt(enc, id); err != nil || string(got) != "archive" {
		t.Errorf("decrypt with the kit: %v", err)
	}

	// A new kit: the first still counts until it's confirmed.
	pass2, _ := s.StartKit()
	kit2, _ := s.PendingKit()
	file, _ = s.Seal([]byte("meanwhile"), Manifest{})
	if _, enc, _ := Open(file, signer); func() bool { _, err := Decrypt(enc, id); return err != nil }() {
		t.Error("a backup made before the new kit's confirmation doesn't open with the first")
	}
	if err := s.ConfirmKit(kit, pass); err == nil {
		t.Error("the first kit confirmed as the new one")
	}
	if err := s.ConfirmKit(kit2, pass2); err != nil {
		t.Fatal(err)
	}
	opened2, _ := OpenKit(kit2, pass2)
	id2, signer2, _ := opened2.Keys()
	if !signer2.Equal(signer) {
		t.Error("the signing key changed with the kit")
	}
	file, _ = s.Seal([]byte("after"), Manifest{})
	_, enc, _ = Open(file, signer)
	if _, err := Decrypt(enc, id); err == nil {
		t.Error("the replaced kit still opens new backups")
	}
	if got, err := Decrypt(enc, id2); err != nil || string(got) != "after" {
		t.Errorf("the new kit: %v", err)
	}

	// Due: once a day after a success, an hour after a failure.
	now := time.Now()
	if !s.Due(now) {
		t.Error("never backed up: not due")
	}
	s.Record(Run{Time: now, Key: "janus/x.janusbackup"})
	if s.Due(now.Add(23*time.Hour)) || !s.Due(now.Add(24*time.Hour)) {
		t.Error("due once a day")
	}
	if a := s.alertLocked(now.Add(47 * time.Hour)); a != "" {
		t.Errorf("a day late: %q", a)
	}
	if a := s.alertLocked(now.Add(49 * time.Hour)); a != AlertLate {
		t.Errorf("two days late: %q", a)
	}
	s.Record(Run{Time: now.Add(24 * time.Hour), Error: "S3: no"})
	if s.Due(now.Add(24*time.Hour+30*time.Minute)) || !s.Due(now.Add(25*time.Hour+time.Minute)) {
		t.Error("an hour after a failure")
	}
	if st := s.Status(); st.Alert != AlertFailed {
		t.Errorf("after a failure: %q", st.Alert)
	}
	s.Record(Run{Time: now.Add(25 * time.Hour), Key: "janus/y.janusbackup"})
	if st := s.Status(); st.Alert != "" {
		t.Errorf("after a success again: %q", st.Alert)
	}
}

func TestSettingsCheck(t *testing.T) {
	for name, set := range map[string]Settings{
		"a bad endpoint":  {Endpoint: "s3.example", IntervalHours: 24},
		"interval 0":      {IntervalHours: 0},
		"keep -1":         {IntervalHours: 24, Keep: -1},
		"a bad recipient": {IntervalHours: 24, Recipients: []string{"hello"}},
	} {
		if err := set.check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	id, _ := age.GenerateX25519Identity()
	if err := (Settings{IntervalHours: 6, Recipients: []string{id.Recipient().String()}}).check(); err != nil {
		t.Error(err)
	}
}
