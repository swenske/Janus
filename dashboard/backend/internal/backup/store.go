package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/swenske/Janus/dashboard/backend/internal/s3"
)

// Settings is where and how often backups go.
type Settings struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the S3 service's URL (https://s3.eu-west-3.amazonaws.com,
	// https://minio.example:9000).
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	PathStyle bool   `json:"path_style"`
	// IntervalHours between backups.
	IntervalHours int `json:"interval_hours"`
	// Keep is how many backups the Controller keeps in the bucket,
	// deleting older ones when its credentials may; 0: never deletes.
	Keep int `json:"keep"`
	// Recipients are admins' keys that can decrypt backups too, besides
	// the kit's: age (age1...) or SSH (ssh-ed25519, ssh-rsa) public keys.
	Recipients []string `json:"recipients"`
}

// DefaultSettings: once a day, the last 30 kept.
var DefaultSettings = Settings{Region: "us-east-1", Prefix: "janus/", PathStyle: true, IntervalHours: 24, Keep: 30}

func (s Settings) check() error {
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("endpoint: the S3 service's URL, https://host[:port]")
		}
	}
	if s.Enabled && (s.Endpoint == "" || s.Bucket == "" || s.AccessKey == "") {
		return errors.New("an endpoint, a bucket and an access key first")
	}
	if s.IntervalHours < 1 || s.IntervalHours > 24*7 {
		return errors.New("interval_hours: 1 to 168")
	}
	if s.Keep < 0 || s.Keep > 1000 {
		return errors.New("keep: 0 (never delete) to 1000")
	}
	for _, r := range s.Recipients {
		if _, err := ParseRecipient(r); err != nil {
			return fmt.Errorf("%q: %w", r, err)
		}
	}
	return nil
}

// Run is one backup's outcome.
type Run struct {
	Time     time.Time `json:"time"`
	Key      string    `json:"key,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Seconds  float64   `json:"seconds"`
	Error    string    `json:"error,omitempty"`
	Deleted  int       `json:"deleted,omitempty"`
	Note     string    `json:"note,omitempty"`
	Manifest *Manifest `json:"manifest,omitempty"`
}

// Kit states.
const (
	KitNone    = "none"
	KitPending = "pending"
	KitReady   = "ready"
)

type state struct {
	Settings      Settings `json:"settings"`
	SecretSealed  []byte   `json:"secret_sealed,omitempty"`
	SigningSealed []byte   `json:"signing_sealed,omitempty"`
	SigningPublic []byte   `json:"signing_public,omitempty"`
	Kit           string   `json:"kit"`
	KitRecipient  string   `json:"kit_recipient,omitempty"`
	// PendingRecipient and PendingKit are a new kit's, until confirmed.
	PendingRecipient string `json:"pending_recipient,omitempty"`
	PendingKit       []byte `json:"pending_kit,omitempty"`
	History          []Run  `json:"history,omitempty"`
}

// Sealer seals the S3 secret and the signing key: the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

const (
	purposeSecret  = "backup-s3-secret"
	purposeSigning = "backup-signing-key"
	historyKept    = 20
)

// Store is the Controller's backup settings and history.
type Store struct {
	path       string
	key        Sealer
	controller string

	mu sync.Mutex
	st state
}

// OpenStore loads dir's state.
func OpenStore(dir string, key Sealer, controller string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "state.json"), key: key, controller: controller, st: state{Settings: DefaultSettings, Kit: KitNone}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Status is what the page shows.
type Status struct {
	Settings  Settings `json:"settings"`
	HasSecret bool     `json:"has_secret"`
	Kit       string   `json:"kit"`
	// KitWaiting: a new kit waits to be confirmed (the previous one, if
	// any, still counts).
	KitWaiting bool `json:"kit_waiting"`
	// SigningKey is the key backups are signed with (base64): what a
	// restore with an admin's key instead of the kit checks them with.
	SigningKey string `json:"signing_key,omitempty"`
	History    []Run  `json:"history"`
	// Next is when the next backup is due, when they're on.
	Next *time.Time `json:"next,omitempty"`
}

func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Settings: s.st.Settings, HasSecret: len(s.st.SecretSealed) > 0, Kit: s.st.Kit, KitWaiting: s.st.PendingKit != nil, History: append([]Run{}, s.st.History...)}
	if len(s.st.SigningPublic) > 0 {
		st.SigningKey = base64.StdEncoding.EncodeToString(s.st.SigningPublic)
	}
	if st.Settings.Recipients == nil {
		st.Settings.Recipients = []string{}
	}
	if next, ok := s.nextLocked(); ok {
		if next.IsZero() {
			next = time.Now()
		}
		st.Next = &next
	}
	return st
}

// SetSettings changes the settings - and the S3 secret, when given.
func (s *Store) SetSettings(set Settings, secret *string) error {
	if err := set.check(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.st
	if secret != nil {
		sealed, err := s.key.Seal([]byte(*secret), purposeSecret)
		if err != nil {
			return err
		}
		s.st.SecretSealed = sealed
	}
	if set.Enabled && len(s.st.SecretSealed) == 0 {
		s.st = old
		return errors.New("the S3 secret key first")
	}
	if set.Enabled && s.st.Kit != KitReady {
		s.st = old
		return errors.New("make the backup kit first: without it, nobody could restore")
	}
	s.st.Settings = set
	if err := s.saveLocked(); err != nil {
		s.st = old
		return err
	}
	return nil
}

// StartKit makes a new backup kit, waiting to be confirmed: its
// passphrase, shown once. A kit made before stops counting once the new
// one is confirmed - backups made meanwhile still open with it.
func (s *Store) StartKit() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.st.SigningSealed) == 0 {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}
		sealed, err := s.key.Seal(priv.Seed(), purposeSigning)
		if err != nil {
			return "", err
		}
		s.st.SigningSealed, s.st.SigningPublic = sealed, pub
	}
	pass, err := newPassphrase()
	if err != nil {
		return "", err
	}
	kit, recipient, err := NewKit(s.controller, s.st.SigningPublic, pass)
	if err != nil {
		return "", err
	}
	old := s.st
	s.st.PendingKit, s.st.PendingRecipient = kit, recipient
	if s.st.Kit != KitReady {
		s.st.Kit = KitPending
	}
	if err := s.saveLocked(); err != nil {
		s.st = old
		return "", err
	}
	return pass, nil
}

// PendingKit is the kit to download, until it's confirmed.
func (s *Store) PendingKit() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingKit == nil {
		return nil, errors.New("no backup kit waits to be downloaded: make a new one")
	}
	return s.st.PendingKit, nil
}

// ConfirmKit checks the kit given back opens with its passphrase and is
// the one just made: from then on, backups are encrypted to it.
func (s *Store) ConfirmKit(kit []byte, passphrase string) error {
	k, err := OpenKit(kit, passphrase)
	if err != nil {
		return err
	}
	id, _, err := k.Keys()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := s.st.PendingRecipient
	if want == "" || id.(*age.X25519Identity).Recipient().String() != want {
		return errors.New("this is another backup kit: give back the one just made")
	}
	old := s.st
	s.st.Kit, s.st.KitRecipient, s.st.PendingKit, s.st.PendingRecipient = KitReady, want, nil, ""
	if err := s.saveLocked(); err != nil {
		s.st = old
		return err
	}
	return nil
}

// sealing is what a backup needs: whom to encrypt it for, and the key to
// sign it with.
func (s *Store) sealing() ([]age.Recipient, ed25519.PrivateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Kit != KitReady {
		return nil, nil, errors.New("make the backup kit first")
	}
	var out []age.Recipient
	r, err := age.ParseX25519Recipient(s.st.KitRecipient)
	if err != nil {
		return nil, nil, err
	}
	out = append(out, r)
	for _, line := range s.st.Settings.Recipients {
		r, err := ParseRecipient(line)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, r)
	}
	seed, err := s.key.Open(s.st.SigningSealed, purposeSigning)
	if err != nil {
		return nil, nil, fmt.Errorf("the signing key: %w", err)
	}
	return out, ed25519.NewKeyFromSeed(seed), nil
}

// Seal encrypts and signs archive.
func (s *Store) Seal(archive []byte, m Manifest) ([]byte, error) {
	recipients, signer, err := s.sealing()
	if err != nil {
		return nil, err
	}
	m.Controller = s.controller
	return Seal(archive, m, recipients, signer)
}

// SigningPublic is the key backups are signed with.
func (s *Store) SigningPublic() ed25519.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(ed25519.PublicKey{}, s.st.SigningPublic...)
}

// Client is the bucket, with the secret unsealed.
func (s *Store) Client() (*s3.Client, string, error) {
	s.mu.Lock()
	set, sealed := s.st.Settings, s.st.SecretSealed
	s.mu.Unlock()
	if set.Endpoint == "" || set.Bucket == "" || len(sealed) == 0 {
		return nil, "", errors.New("no S3 bucket set up")
	}
	secret, err := s.key.Open(sealed, purposeSecret)
	if err != nil {
		return nil, "", fmt.Errorf("the S3 secret: %w", err)
	}
	u, err := url.Parse(set.Endpoint)
	if err != nil {
		return nil, "", err
	}
	region := set.Region
	if region == "" {
		region = "us-east-1"
	}
	return &s3.Client{Endpoint: u, Region: region, Bucket: set.Bucket, AccessKey: set.AccessKey, SecretKey: string(secret), PathStyle: set.PathStyle}, set.Prefix, nil
}

// Record keeps a run in the history.
func (s *Store) Record(r Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.History = append([]Run{r}, s.st.History...)
	if len(s.st.History) > historyKept {
		s.st.History = s.st.History[:historyKept]
	}
	_ = s.saveLocked() // the history is informational
}

// nextLocked is when the next backup is due - the last success plus the
// interval - when backups are on.
func (s *Store) nextLocked() (time.Time, bool) {
	set := s.st.Settings
	if !set.Enabled || s.st.Kit != KitReady {
		return time.Time{}, false
	}
	for _, r := range s.st.History {
		if r.Error == "" && r.Key != "" {
			return r.Time.Add(time.Duration(set.IntervalHours) * time.Hour), true
		}
	}
	return time.Time{}, true // never backed up: at once
}

// Due reports whether a backup should run now.
func (s *Store) Due(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, ok := s.nextLocked()
	if !ok {
		return false
	}
	// After a failure, try again in an hour rather than every minute.
	if len(s.st.History) > 0 && s.st.History[0].Error != "" && now.Sub(s.st.History[0].Time) < time.Hour {
		return false
	}
	return !now.Before(next)
}

// Keep is how many backups to keep (0: never delete).
func (s *Store) Keep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Settings.Keep
}

const passphraseAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newPassphrase is six groups of four Crockford base 32 characters (120
// bits), like the fleet's recovery kit.
func newPassphrase() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(passphraseAlphabet[int(c)%len(passphraseAlphabet)])
	}
	return sb.String(), nil
}
