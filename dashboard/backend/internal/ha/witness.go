package ha

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/s3"
)

// The witness (docs/guide/controller-standby.md, "The witness"): a record
// where both Controllers of the pair reach even when they can't reach
// each other - the Controller's Consul KV when its Consul settings are
// on, else its backups' S3 bucket - saying which of the two holds which
// epoch, signed with that one's HA key. A standby being promoted writes
// it first, at its new epoch; the active reads it at its start and every
// 30 s, and steps down when its peer holds an epoch above its own: a
// promotion forced while the old active still runs behind a partition
// stops it within 30 s, as long as it reaches the witness.
//
// Writes are conditional - Consul's check-and-set on the key's
// ModifyIndex, S3's If-Match on the ETag read (If-None-Match when there
// was none) -: of two writers, one loses and reads again. An S3 service
// that refuses conditional writes gets plain ones.

// Witness is where the witness lives.
type Witness interface {
	// Read is the record and its version - nil, "" when there's none. An
	// object there that isn't a record comes back as an empty one, with
	// its version: nobody's, written over.
	Read(ctx context.Context) (*WitnessRecord, string, error)
	// Write puts rec over the version read ("": there was none). It
	// loses (ErrWitnessChanged) when another writer came between;
	// unconditional: the store refused a conditional write and got a
	// plain one.
	Write(ctx context.Context, rec WitnessRecord, version string) (unconditional bool, err error)
	// Where says where it is, for the operator.
	Where() string
}

// ErrWitnessChanged is a write that lost: another came between.
var ErrWitnessChanged = errors.New("the witness changed while this Controller wrote it")

// WitnessKey is the witness's S3 object for a Controller's ID - the pair
// shares it: a copy carries it -, beside its backups.
func WitnessKey(prefix, controllerID string) string {
	return prefix + "janus-controller-" + shortID(controllerID) + ".witness.json"
}

// ConsulWitnessKey is the witness's Consul key: what the Controller's
// token needs key_prefix "janus/controllers/" write for.
func ConsulWitnessKey(controllerID string) string {
	return "janus/controllers/" + shortID(controllerID) + "/witness"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// WitnessRecord is the object's content.
type WitnessRecord struct {
	Epoch uint64 `json:"epoch"`
	// Holder is the holder's HA certificate's SHA-256; Name what it calls
	// itself; At when it wrote the record.
	Holder string    `json:"holder"`
	Name   string    `json:"name"`
	At     time.Time `json:"at"`
	// CertPEM is the holder's HA certificate, Signature its key's (ECDSA,
	// ASN.1, base64) over the fields above.
	CertPEM   string `json:"cert_pem"`
	Signature string `json:"signature"`
}

func (w WitnessRecord) signed() []byte {
	digest := sha256.Sum256(fmt.Appendf(nil, "janus-ha-witness/1\n%d\n%s\n%s\n%s\n", w.Epoch, w.Holder, w.Name, w.At.UTC().Format(time.RFC3339Nano)))
	return digest[:]
}

// SignWitness is this Controller's record holding epoch.
func (s *Store) SignWitness(epoch uint64, name string, at time.Time) (WitnessRecord, error) {
	w := WitnessRecord{Epoch: epoch, Holder: s.certFP, Name: name, At: at.UTC(), CertPEM: string(s.CertPEM())}
	signer, ok := s.cert.PrivateKey.(crypto.Signer)
	if !ok {
		return w, errors.New("the HA key can't sign")
	}
	sig, err := signer.Sign(rand.Reader, w.signed(), crypto.SHA256)
	if err != nil {
		return w, err
	}
	w.Signature = base64.StdEncoding.EncodeToString(sig)
	return w, nil
}

// Whose a record is.
const (
	HolderSelf = "self"
	HolderPeer = "peer"
)

// CheckWitness says whose w is - this Controller's (HolderSelf) or its
// peer's (HolderPeer) - or why it's neither's: not signed by the
// certificate it carries, or held by a Controller this pair doesn't
// know (another pair's, a peer since removed, a forgery).
func (s *Store) CheckWitness(w WitnessRecord) (string, error) {
	block, _ := pem.Decode([]byte(w.CertPEM))
	if block == nil {
		return "", errors.New("the witness carries no certificate")
	}
	if fingerprint(block.Bytes) != w.Holder {
		return "", errors.New("the witness's certificate isn't its holder's")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("the witness's certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	sig, err := base64.StdEncoding.DecodeString(w.Signature)
	if !ok || err != nil || !ecdsa.VerifyASN1(pub, w.signed(), sig) {
		return "", errors.New("the witness's signature doesn't verify")
	}
	s.mu.Lock()
	peer := s.state.Peer
	s.mu.Unlock()
	switch {
	case w.Holder == s.certFP:
		return HolderSelf, nil
	case peer != nil && w.Holder == peer.CertSHA256:
		return HolderPeer, nil
	}
	return "", fmt.Errorf("the witness is held by %q (%.12s...), neither this Controller nor its peer", w.Name, w.Holder)
}

// Verdicts on the witness, an active's.
type Verdict int

const (
	// WitnessKeep: it says this Controller holds its epoch.
	WitnessKeep Verdict = iota
	// WitnessWrite: none, or this pair's but behind, or nobody's this
	// pair knows: the active writes its own.
	WitnessWrite
	// WitnessStepDown: the peer holds an epoch above the active's - or
	// the same one, which the peer won.
	WitnessStepDown
)

// JudgeWitness is an active's verdict on the witness w (nil: there's
// none), whose CheckWitness found it ("": nobody's of this pair).
func JudgeWitness(own State, w *WitnessRecord, holder string) Verdict {
	switch {
	case w == nil || holder == "":
		return WitnessWrite
	case holder == HolderPeer && w.Epoch >= own.Epoch:
		return WitnessStepDown
	case w.Epoch < own.Epoch:
		return WitnessWrite
	}
	return WitnessKeep
}

// Bucket is the little of an S3 client the witness needs (*s3.Client).
type Bucket interface {
	GetETag(ctx context.Context, key string) ([]byte, string, error)
	PutIf(ctx context.Context, key string, body []byte, contentType string, cond s3.Condition) error
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

// S3Witness is the witness as an object in the backups' bucket.
type S3Witness struct {
	Bucket Bucket
	Key    string
	// Name is the bucket, for Where.
	Name string
}

func (w S3Witness) Where() string { return "s3://" + w.Name + "/" + w.Key }

func (w S3Witness) Read(ctx context.Context) (*WitnessRecord, string, error) {
	data, etag, err := w.Bucket.GetETag(ctx, w.Key)
	if s3.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return parseRecord(data), etag, nil
}

func (w S3Witness) Write(ctx context.Context, rec WitnessRecord, etag string) (bool, error) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return false, err
	}
	err = w.Bucket.PutIf(ctx, w.Key, data, "application/json", s3.Condition{IfMatch: etag, IfNoneMatch: etag == ""})
	var e *s3.Error
	switch {
	case s3.IsPreconditionFailed(err):
		return false, ErrWitnessChanged
	case errors.As(err, &e) && (e.Status == http.StatusNotImplemented || e.Code == "NotImplemented"):
		return true, w.Bucket.Put(ctx, w.Key, data, "application/json")
	}
	return false, err
}

// KV is the little of a Consul client the witness needs
// (*consulctl.Client).
type KV interface {
	KVGet(ctx context.Context, key string) ([]byte, uint64, error)
	KVPutCAS(ctx context.Context, key string, value json.RawMessage, index uint64) (bool, error)
}

// ConsulWitness is the witness as a key in the Controller's Consul KV.
type ConsulWitness struct {
	KV  KV
	Key string
	// Server is the Consul server's address, for Where; NoKey what its
	// KVGet answers for a missing key.
	Server string
	NoKey  error
}

func (w ConsulWitness) Where() string { return "consul " + w.Server + " key " + w.Key }

func (w ConsulWitness) Read(ctx context.Context) (*WitnessRecord, string, error) {
	data, index, err := w.KV.KVGet(ctx, w.Key)
	if err != nil && w.NoKey != nil && errors.Is(err, w.NoKey) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return parseRecord(data), strconv.FormatUint(index, 10), nil
}

func (w ConsulWitness) Write(ctx context.Context, rec WitnessRecord, version string) (bool, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	var index uint64
	if version != "" {
		if index, err = strconv.ParseUint(version, 10, 64); err != nil || index == 0 {
			return false, fmt.Errorf("the witness's version %q", version)
		}
	}
	ok, err := w.KV.KVPutCAS(ctx, w.Key, data, index)
	if err == nil && !ok {
		err = ErrWitnessChanged
	}
	return false, err
}

// parseRecord is a record, or an empty one - nobody's - for what isn't.
func parseRecord(data []byte) *WitnessRecord {
	var rec WitnessRecord
	if json.Unmarshal(data, &rec) != nil {
		return &WitnessRecord{}
	}
	return &rec
}

// ClaimWitness makes this Controller the witness's holder at want, or
// above what the witness says this pair reached: the epoch it holds.
// What a standby being promoted does first.
func (s *Store) ClaimWitness(ctx context.Context, w Witness, want uint64, name string) (uint64, error) {
	for range 3 {
		rec, version, err := w.Read(ctx)
		if err != nil {
			return 0, err
		}
		epoch := want
		if rec != nil {
			if _, err := s.CheckWitness(*rec); err == nil && rec.Epoch >= epoch {
				epoch = rec.Epoch + 1
			}
		}
		signed, err := s.SignWitness(epoch, name, time.Now())
		if err != nil {
			return 0, err
		}
		if _, err := w.Write(ctx, signed, version); errors.Is(err, ErrWitnessChanged) {
			continue
		} else if err != nil {
			return 0, err
		}
		return epoch, nil
	}
	return 0, ErrWitnessChanged
}
