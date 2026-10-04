package api

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The node's secrets are never served by the file RPCs (Read, Copy): its
// PKI's private keys, the Controller registration token, the ACME
// account key and DNS provider credentials, the private keys of HAProxy's
// certificates. No other API returns them either, and an os:admin
// certificate that could carry them away would keep its access - or
// impersonate the node - long after it expired: the node CA's key signs
// certificates valid for a year. List and DiskUsage still show their
// names and sizes.
//
// Two checks. SecretPaths, matched against the path the kernel resolved
// for the open file (/proc/self/fd), so a symlink or /proc/self/root
// can't go around them; and the content of every other file: a PEM
// private key block anywhere is refused, wherever the file lives.

// SecretPaths are files, or directories whose whole content is, never
// served - each as seen through its bind mount and on STATE itself
// (rootfs/init's mountState). cmd/janusd adds its -pki-dir.
var SecretPaths = []string{
	"/etc/janus/pki", "/etc/.state/pki", // CA, server and admin keys
	"/etc/janus/controller/token", "/etc/.state/controller/token", // registration token
	"/etc/janus/controller/enrollment.json", "/etc/.state/controller/enrollment.json", // the enrollment's poll secret
	"/etc/janus/config/acme", "/etc/.state/config/acme", // ACME account key
	"/etc/janus/config/letsencrypt.json", "/etc/.state/config/letsencrypt.json", // DNS provider credentials, EAB key
}

// privateKeyBlock is the line opening a PEM private key: "PRIVATE KEY",
// "EC PRIVATE KEY", "OPENSSH PRIVATE KEY", "ENCRYPTED PRIVATE KEY"...
var privateKeyBlock = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY-----`)

// keyBlockMax is the longest text privateKeyBlock matches: a reader keeps
// that much of the previous chunk minus one, to see a marker split
// across two reads.
const keyBlockMax = len("-----BEGIN ") + 40 + len("PRIVATE KEY-----")

var errPrivateKey = errors.New("holds a private key")

// isSecretPath reports whether path is one of SecretPaths or under one.
func isSecretPath(path string) bool {
	for _, s := range SecretPaths {
		s = filepath.Clean(s)
		if path == s || strings.HasPrefix(path, s+"/") {
			return true
		}
	}
	return false
}

// openedPath is the path the kernel resolved for f.
func openedPath(f *os.File) (string, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var path string
	var linkErr error
	if err := sc.Control(func(fd uintptr) {
		path, linkErr = os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10))
	}); err != nil {
		return "", err
	}
	return path, linkErr
}

// checkServable reports why the regular file f must not be served - nil
// when it can be. It reads f through and leaves it at its start again.
func checkServable(f *os.File) error {
	opened, err := openedPath(f)
	if err != nil {
		return err // fail closed: the location can't be told
	}
	if isSecretPath(opened) {
		return errSecretPath
	}
	if _, err := io.Copy(io.Discard, &keyGuard{r: f}); err != nil {
		return err
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

var errSecretPath = errors.New("is one of the node's secrets")

// keyGuard passes r through, but fails with errPrivateKey - before
// handing over the chunk that holds it, or anything after - as soon as a
// PEM private key block shows up: what was checked once may have changed
// by the time it's sent.
type keyGuard struct {
	r    io.Reader
	tail []byte
}

func (g *keyGuard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n > 0 {
		seen := append(g.tail, p[:n]...)
		if privateKeyBlock.Match(seen) {
			return 0, errPrivateKey
		}
		keep := min(len(seen), keyBlockMax-1)
		g.tail = append(g.tail[:0], seen[len(seen)-keep:]...)
	}
	return n, err
}
