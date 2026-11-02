package ha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/s3"
)

// pair is two stores paired: a the active, b its standby.
func pair(t *testing.T) (a, b *Store) {
	t.Helper()
	a, b = open(t, t.TempDir(), newKey(t)), open(t, t.TempDir(), newKey(t))
	secret, _ := NewSecret()
	if err := a.SetPeer(Peer{Name: "b", Address: "b:8443", CertSHA256: b.CertSHA256()}, secret, Active, 4); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPeer(Peer{Name: "a", Address: "a:8443", CertSHA256: a.CertSHA256()}, secret, Standby, 4); err != nil {
		t.Fatal(err)
	}
	return a, b
}

// TestWitnessSigned: a record is its holder's, signed by its HA key -
// each of the pair knows whose; another Controller's, or one changed, is
// nobody's.
func TestWitnessSigned(t *testing.T) {
	a, b := pair(t)
	rec, err := a.SignWitness(5, "a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if h, err := a.CheckWitness(rec); h != HolderSelf || err != nil {
		t.Errorf("a's own: %q %v", h, err)
	}
	if h, err := b.CheckWitness(rec); h != HolderPeer || err != nil {
		t.Errorf("a's, to b: %q %v", h, err)
	}
	other := open(t, t.TempDir(), newKey(t))
	if h, err := other.CheckWitness(rec); h != "" || err == nil || !strings.Contains(err.Error(), "neither") {
		t.Errorf("a's, to a stranger: %q %v", h, err)
	}
	changed := rec
	changed.Epoch = 9
	if _, err := b.CheckWitness(changed); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("an epoch changed: %v", err)
	}
	stranger, _ := other.SignWitness(9, "a", time.Now())
	forged := stranger
	forged.Holder = a.CertSHA256()
	if _, err := b.CheckWitness(forged); err == nil {
		t.Error("a stranger's record naming a as its holder passed")
	}
	if _, err := b.CheckWitness(WitnessRecord{}); err == nil {
		t.Error("an empty record passed")
	}
}

// TestJudgeWitness: what an active makes of the witness.
func TestJudgeWitness(t *testing.T) {
	own := State{Role: Active, Epoch: 4}
	rec := func(e uint64) *WitnessRecord { return &WitnessRecord{Epoch: e} }
	for _, c := range []struct {
		name   string
		w      *WitnessRecord
		holder string
		want   Verdict
	}{
		{"none", nil, "", WitnessWrite},
		{"nobody's", rec(9), "", WitnessWrite},
		{"its own, current", rec(4), HolderSelf, WitnessKeep},
		{"its own, behind", rec(3), HolderSelf, WitnessWrite},
		{"its own, ahead", rec(5), HolderSelf, WitnessKeep},
		{"the peer's, behind", rec(3), HolderPeer, WitnessWrite},
		{"the peer's, the same epoch", rec(4), HolderPeer, WitnessStepDown},
		{"the peer's, above", rec(5), HolderPeer, WitnessStepDown},
	} {
		if got := JudgeWitness(own, c.w, c.holder); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// memKV is a Consul KV in memory: ModifyIndex, check-and-set.
type memKV struct {
	mu    sync.Mutex
	data  map[string][]byte
	index map[string]uint64
	next  uint64
	// race, when set, writes in between a read and the next CAS once.
	race func()
	fail error
}

var errNoKey = errors.New("no such key")

func (m *memKV) KVGet(_ context.Context, key string) ([]byte, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, 0, m.fail
	}
	if _, ok := m.data[key]; !ok {
		return nil, 0, errNoKey
	}
	return m.data[key], m.index[key], nil
}

func (m *memKV) KVPutCAS(_ context.Context, key string, value json.RawMessage, index uint64) (bool, error) {
	if r := m.race; r != nil {
		m.race = nil
		r()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	if m.index[key] != index {
		return false, nil
	}
	m.next++
	m.data[key], m.index[key] = append([]byte(nil), value...), m.next
	return true, nil
}

func newKV() *memKV { return &memKV{data: map[string][]byte{}, index: map[string]uint64{}, next: 10} }

// memBucket is an S3 bucket in memory: ETags, If-Match, If-None-Match -
// or none of them (noConditions: 501).
type memBucket struct {
	mu           sync.Mutex
	data         map[string][]byte
	etag         map[string]string
	n            int
	noConditions bool
}

func (m *memBucket) GetETag(_ context.Context, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[key]
	if !ok {
		return nil, "", &s3.Error{Status: http.StatusNotFound, Code: "NoSuchKey"}
	}
	return d, m.etag[key], nil
}

func (m *memBucket) PutIf(ctx context.Context, key string, body []byte, ct string, cond s3.Condition) error {
	m.mu.Lock()
	if m.noConditions {
		m.mu.Unlock()
		return &s3.Error{Status: http.StatusNotImplemented, Code: "NotImplemented"}
	}
	_, exists := m.data[key]
	if (cond.IfNoneMatch && exists) || (cond.IfMatch != "" && cond.IfMatch != m.etag[key]) {
		m.mu.Unlock()
		return &s3.Error{Status: http.StatusPreconditionFailed, Code: "PreconditionFailed"}
	}
	m.mu.Unlock()
	return m.Put(ctx, key, body, ct)
}

func (m *memBucket) Put(_ context.Context, key string, body []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	m.data[key], m.etag[key] = append([]byte(nil), body...), `"`+strconv.Itoa(m.n)+`"`
	return nil
}

func newBucket() *memBucket { return &memBucket{data: map[string][]byte{}, etag: map[string]string{}} }

// TestClaimWitness: a standby being promoted claims the witness - in
// Consul's KV or an S3 bucket alike - above what it says; a writer in
// between makes it read again; the active it replaced then finds its
// peer above it.
func TestClaimWitness(t *testing.T) {
	ctx := context.Background()
	kv := newKV()
	for name, w := range map[string]Witness{
		"consul": ConsulWitness{KV: kv, Key: ConsulWitnessKey("0123456789abcdef"), Server: "https://consul", NoKey: errNoKey},
		"s3":     S3Witness{Bucket: newBucket(), Key: WitnessKey("janus/", "0123456789abcdef"), Name: "b"},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := pair(t)
			if rec, v, err := w.Read(ctx); rec != nil || v != "" || err != nil {
				t.Fatalf("an empty witness: %v %q %v", rec, v, err)
			}
			// The active writes its epoch.
			mine, _ := a.SignWitness(4, "a", time.Now())
			if _, err := w.Write(ctx, mine, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(ctx, mine, ""); !errors.Is(err, ErrWitnessChanged) {
				t.Fatalf("a second write as if there were none: %v", err)
			}
			// The standby claims 5; the witness holds it.
			got, err := b.ClaimWitness(ctx, w, 5, "b")
			if err != nil || got != 5 {
				t.Fatalf("claim: %d %v", got, err)
			}
			rec, _, err := w.Read(ctx)
			if err != nil || rec.Epoch != 5 {
				t.Fatalf("after the claim: %+v %v", rec, err)
			}
			h, err := a.CheckWitness(*rec)
			if err != nil || JudgeWitness(a.State(), rec, h) != WitnessStepDown {
				t.Fatalf("the old active: %q %v", h, err)
			}
			// A claim below what the witness says goes above it.
			if got, err := b.ClaimWitness(ctx, w, 3, "b"); err != nil || got != 6 {
				t.Fatalf("a claim below the witness: %d %v", got, err)
			}
		})
	}
	// A writer in between: the claim reads again and goes above it.
	a, b := pair(t)
	w := ConsulWitness{KV: kv, Key: "janus/controllers/race/witness", NoKey: errNoKey}
	kv.race = func() {
		rec, _ := a.SignWitness(7, "a", time.Now())
		data, _ := json.Marshal(rec)
		kv.data[w.Key], kv.index[w.Key] = data, 99
	}
	if got, err := b.ClaimWitness(ctx, w, 5, "b"); err != nil || got != 8 {
		t.Fatalf("a writer in between: %d %v", got, err)
	}
	kv.fail = errors.New("connection refused")
	if _, err := b.ClaimWitness(ctx, w, 9, "b"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Consul down: %v", err)
	}
	// An S3 service without conditional writes: written plainly, said so.
	plain := S3Witness{Bucket: &memBucket{data: map[string][]byte{}, etag: map[string]string{}, noConditions: true}, Key: "w"}
	rec, _ := b.SignWitness(1, "b", time.Now())
	if unconditional, err := plain.Write(ctx, rec, ""); !unconditional || err != nil {
		t.Fatalf("no conditional writes: %v %v", unconditional, err)
	}
	// What isn't a record is nobody's: written over.
	junk := newBucket()
	_ = junk.Put(ctx, "w", []byte("not json"), "text/plain")
	got, v, err := S3Witness{Bucket: junk, Key: "w"}.Read(ctx)
	if err != nil || got == nil || v == "" {
		t.Fatalf("junk: %v %q %v", got, v, err)
	}
	if h, _ := a.CheckWitness(*got); JudgeWitness(a.State(), got, h) != WitnessWrite {
		t.Fatal("junk isn't written over")
	}
}
