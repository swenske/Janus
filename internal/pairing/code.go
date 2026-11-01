// Package pairing is a node's pairing window: how a node that trusts
// nothing yet - no fleet, no Controller - is given a fleet without a
// credential ever shown on its console (docs/internals/trust.md).
//
// janusd opens it on a port of its own, TLS with a certificate made for
// the window (a key of its own, signed by the node's CA), no client
// certificate asked, and shows a code on the console:
//
//	pairing  9BC3-3016-3D39-2K7Q-M2XD
//
// Twenty Crockford base32 symbols, 100 bits: the first 59 bits of the
// window certificate's SHA-256 - a client checks the certificate the
// node presents against them before it sends anything (finding another
// key whose certificate has the same 59 bits takes 2^59 tries, the
// window lasts until the node restarts) -, then a 36-bit secret - the
// proof that the client read the console, five wrong ones closing the
// window -, then a 5-bit check that catches a mistyped code before it
// costs a try. The code is new at each start, kept in memory only, and
// good once: it isn't a key, there's nothing to keep.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	// Symbols is a code's length, without its dashes.
	Symbols    = 20
	prefixBits = 59
	secretBits = 36
	checkBits  = 5
	alphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford's
)

// Code is a pairing code: what names the window's certificate, and the
// secret.
type Code struct {
	Prefix uint64 // prefixBits of the certificate's SHA-256
	Secret uint64 // secretBits
}

// PrefixOf is the first 59 bits of der's SHA-256 - what a code names a
// certificate by.
func PrefixOf(der []byte) uint64 {
	sum := sha256.Sum256(der)
	return binary.BigEndian.Uint64(sum[:8]) >> (64 - prefixBits)
}

// NewCode is a code for the certificate der, its secret random.
func NewCode(der []byte) (Code, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Code{}, err
	}
	return Code{Prefix: PrefixOf(der), Secret: binary.BigEndian.Uint64(b[:]) >> (64 - secretBits)}, nil
}

// Names reports whether the code names the certificate der.
func (c Code) Names(der []byte) bool { return c.Prefix == PrefixOf(der) }

// SecretBytes is the secret as PairRequest carries it: 5 bytes, big
// endian.
func (c Code) SecretBytes() []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], c.Secret)
	return b[3:]
}

// check is the 5 bits that catch a mistyped code: the first ones of the
// SHA-256 of the 95 others.
func (c Code) check() uint64 {
	v := new(big.Int).Lsh(new(big.Int).SetUint64(c.Prefix), secretBits)
	v.Or(v, new(big.Int).SetUint64(c.Secret))
	var b [12]byte
	v.FillBytes(b[:])
	sum := sha256.Sum256(b[:])
	return uint64(sum[0] >> (8 - checkBits))
}

// String is the code as the console shows it: five groups of four.
func (c Code) String() string {
	v := new(big.Int).Lsh(new(big.Int).SetUint64(c.Prefix), secretBits+checkBits)
	v.Or(v, new(big.Int).Lsh(new(big.Int).SetUint64(c.Secret), checkBits))
	v.Or(v, new(big.Int).SetUint64(c.check()))
	var out [Symbols]byte
	mask := big.NewInt(31)
	for i := Symbols - 1; i >= 0; i-- {
		out[i] = alphabet[new(big.Int).And(v, mask).Int64()]
		v.Rsh(v, 5)
	}
	var s strings.Builder
	for i, ch := range out {
		if i > 0 && i%4 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(ch)
	}
	return s.String()
}

// ErrMistyped is a code whose check doesn't match: a symbol wrong, or
// two swapped.
var ErrMistyped = errors.New("the pairing code doesn't check: a symbol mistyped - read it again on the node's console")

// ParseCode reads a code as a person types it: dashes and spaces
// anywhere, any case, O for 0, I and L for 1.
func ParseCode(s string) (Code, error) {
	v := new(big.Int)
	n := 0
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		i := strings.IndexRune(alphabet, r)
		if i < 0 {
			return Code{}, fmt.Errorf("the pairing code has %q, not a symbol of its alphabet", r)
		}
		v.Lsh(v, 5)
		v.Or(v, big.NewInt(int64(i)))
		n++
	}
	if n != Symbols {
		return Code{}, fmt.Errorf("a pairing code has %d symbols, not %d", Symbols, n)
	}
	check := new(big.Int).And(v, big.NewInt(1<<checkBits-1)).Uint64()
	v.Rsh(v, checkBits)
	secret := new(big.Int).And(v, new(big.Int).SetUint64(1<<secretBits-1)).Uint64()
	v.Rsh(v, secretBits)
	c := Code{Prefix: v.Uint64(), Secret: secret}
	if c.check() != check {
		return Code{}, ErrMistyped
	}
	return c, nil
}
