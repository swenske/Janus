package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// artifact is one downloaded release file being bumped to.
type artifact struct {
	version string // the new version, as upstream writes it
	arch    string // "" for an architecture-independent artifact
	url     string
	path    string // the downloaded file
	sha256  string
}

// A check proves an artifact is upstream's own. Checks run before
// versions.mk is touched: one failing refuses the bump.
type check interface {
	describe() string
	verify(ctx context.Context, f fetcher, a *artifact) error
}

// errNoCrossCheck: the independent source doesn't know this version yet.
var errNoCrossCheck = errors.New("no independent checksum for this version yet")

// keysDir holds the OpenPGP public keys signatures are checked against,
// one armored file per key, named after its fingerprint.
var keysDir = filepath.Join("hack", "upstream", "keys")

// gpgSig checks a detached signature by one of the given keys. decompress
// "xz" checks it over the decompressed file (the kernel signs its tar, not
// the .tar.xz).
type gpgSig struct {
	sigURL     func(a *artifact) string
	keys       []string // primary key fingerprints
	decompress string
}

func (g gpgSig) describe() string {
	return "OpenPGP signature by " + strings.Join(shortFprs(g.keys), " or ")
}

func (g gpgSig) verify(ctx context.Context, f fetcher, a *artifact) error {
	sig, err := f.download(ctx, g.sigURL(a))
	if err != nil {
		return err
	}
	var data io.Reader
	if g.decompress != "" {
		file, err := os.Open(a.path)
		if err != nil {
			return err
		}
		defer file.Close()
		data = file
	}
	_, err = gpgVerify(g.keys, sig, a.path, data, g.decompress)
	return err
}

// gpgVerify runs gpgv against a keyring holding only the expected keys,
// and checks the primary key that made the signature is one of them. With
// no path, sig is a clearsigned file, whose signed text is returned; with
// compressed, the signature covers that stream, decompressed.
func gpgVerify(keys []string, sig, path string, compressed io.Reader, decompress string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "upstream-gpg-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	keyring := filepath.Join(dir, "keyring.gpg")
	out, err := os.Create(keyring)
	if err != nil {
		return nil, err
	}
	for _, fpr := range keys {
		armored, err := os.ReadFile(filepath.Join(keysDir, fpr+".asc"))
		if err != nil {
			out.Close()
			return nil, fmt.Errorf("key %s: %w", fpr, err)
		}
		cmd := exec.Command("gpg", "--batch", "--dearmor")
		cmd.Stdin = strings.NewReader(string(armored))
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			out.Close()
			return nil, fmt.Errorf("gpg --dearmor %s: %w", fpr, err)
		}
	}
	if err := out.Close(); err != nil {
		return nil, err
	}

	args := []string{"--status-fd", "1", "--keyring", keyring}
	plain := filepath.Join(dir, "plain")
	if path == "" {
		args = append(args, "--output", plain, sig)
	} else if compressed != nil {
		args = append(args, sig, "-")
	} else {
		args = append(args, sig, path)
	}
	cmd := exec.Command("gpgv", args...)
	if compressed != nil {
		if decompress != "xz" {
			return nil, fmt.Errorf("unknown decompression %q", decompress)
		}
		dec := exec.Command("xz", "-dc")
		dec.Stdin = compressed
		pipe, err := dec.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := dec.Start(); err != nil {
			return nil, err
		}
		defer func() { _ = dec.Wait() }()
		cmd.Stdin = pipe
	}
	status, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("signature %s doesn't verify: %v", filepath.Base(sig), err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "[GNUPG:]" && fields[1] == "VALIDSIG" {
			primary := fields[len(fields)-1]
			if !slices.Contains(keys, primary) {
				return nil, fmt.Errorf("signed by %s, not by an expected key", primary)
			}
			if path != "" {
				return nil, nil
			}
			return os.ReadFile(plain)
		}
	}
	return nil, fmt.Errorf("signature %s: gpgv reported no valid signature", filepath.Base(sig))
}

func shortFprs(fprs []string) []string {
	out := make([]string, len(fprs))
	for i, f := range fprs {
		out[i] = f[len(f)-16:]
	}
	return out
}

// signedSums checks the artifact against a checksum list that is itself
// signed by one of the given keys - by a detached signature (sigURL), or
// clearsigned (no sigURL).
type signedSums struct {
	sumsURL, sigURL func(a *artifact) string
	keys            []string
}

func (s signedSums) describe() string {
	return "checksum list signed by " + strings.Join(shortFprs(s.keys), " or ")
}

func (s signedSums) verify(ctx context.Context, f fetcher, a *artifact) error {
	sums, err := f.get(ctx, s.sumsURL(a))
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "upstream-sums-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sumsPath := filepath.Join(dir, "sums")
	if err := os.WriteFile(sumsPath, sums, 0o644); err != nil {
		return err
	}
	if s.sigURL == nil {
		if sums, err = gpgVerify(s.keys, sumsPath, "", nil, ""); err != nil {
			return err
		}
	} else {
		sig, err := f.get(ctx, s.sigURL(a))
		if err != nil {
			return err
		}
		sigPath := filepath.Join(dir, "sig")
		if err := os.WriteFile(sigPath, sig, 0o644); err != nil {
			return err
		}
		if _, err := gpgVerify(s.keys, sigPath, sumsPath, nil, ""); err != nil {
			return err
		}
	}
	return matchSum(string(sums), filepath.Base(a.url), a.sha256)
}

// publishedSum checks the artifact against a checksum upstream publishes
// over HTTPS: a "<sha256>  <name>" list, or a file with just the sum.
type publishedSum struct {
	sumURL func(a *artifact) string
}

func (publishedSum) describe() string { return "sha256 published by upstream (HTTPS)" }

func (p publishedSum) verify(ctx context.Context, f fetcher, a *artifact) error {
	data, err := f.get(ctx, p.sumURL(a))
	if err != nil {
		return err
	}
	return matchSum(string(data), filepath.Base(a.url), a.sha256)
}

// matchSum finds name's sha256 in a checksum list (or takes the only sum
// of a file that names no file) and compares it.
func matchSum(list, name, sum string) error {
	var only []string
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 1 {
			only = append(only, fields[0])
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			if !strings.EqualFold(fields[0], sum) {
				return fmt.Errorf("%s: sha256 %s, upstream publishes %s", name, sum, fields[0])
			}
			return nil
		}
	}
	if len(only) == 1 {
		if !strings.EqualFold(only[0], sum) {
			return fmt.Errorf("%s: sha256 %s, upstream publishes %s", name, sum, only[0])
		}
		return nil
	}
	return fmt.Errorf("%s: not in upstream's checksum list", name)
}

// haproxySum checks the tarball against the sha256 of the branch's
// releases.json, besides the .sha256 file next to it.
type haproxySum struct{}

func (haproxySum) describe() string { return "sha256 in haproxy.org's releases.json (HTTPS)" }

func (haproxySum) verify(ctx context.Context, f fetcher, a *artifact) error {
	doc, err := haproxyBranchReleases(ctx, f, a.version)
	if err != nil {
		return err
	}
	rel, ok := doc.Releases[a.version]
	if !ok {
		return fmt.Errorf("releases.json has no %s", a.version)
	}
	if !strings.EqualFold(rel.SHA256, a.sha256) {
		return fmt.Errorf("sha256 %s, releases.json says %s", a.sha256, rel.SHA256)
	}
	return nil
}

// githubDigest checks the artifact against the digest GitHub computed for
// the release asset when it was uploaded. tag makes the release's tag from
// the version: "v%s" from its plain number, "%s" as it is pinned.
type githubDigest struct {
	repo, tag string
}

func releaseTag(format, v string) string {
	if strings.HasPrefix(format, "v") {
		v = plain(v)
	}
	return fmt.Sprintf(format, v)
}

func (githubDigest) describe() string { return "GitHub's digest of the release asset" }

func (g githubDigest) verify(ctx context.Context, f fetcher, a *artifact) error {
	rel, err := gitHubRelease(ctx, f, g.repo, releaseTag(g.tag, a.version))
	if err != nil {
		return err
	}
	for _, asset := range rel.Assets {
		if asset.URL == a.url {
			want, ok := strings.CutPrefix(asset.Digest, "sha256:")
			if !ok {
				return fmt.Errorf("%s: GitHub gives no sha256 digest", asset.Name)
			}
			if !strings.EqualFold(want, a.sha256) {
				return fmt.Errorf("%s: sha256 %s, GitHub says %s", asset.Name, a.sha256, want)
			}
			return nil
		}
	}
	return fmt.Errorf("release %s has no asset %s", a.version, a.url)
}

// alpineAPKBUILD cross-checks the artifact against the sha512 Alpine pins
// for the same upstream file - an independent party that downloaded it
// separately. Alpine's edge (aports master, its GitHub mirror) is read; file names the file
// there when Alpine saves it under another name.
type alpineAPKBUILD struct {
	path string // "main/zlib"
	file func(v string) string
}

func (a alpineAPKBUILD) describe() string { return "Alpine's pinned sha512 (aports " + a.path + ")" }

var (
	pkgverRe      = regexp.MustCompile(`(?m)^pkgver=(\S+)`)
	sha512blockRe = regexp.MustCompile(`(?s)sha512sums="(.*?)"`)
)

func (al alpineAPKBUILD) verify(ctx context.Context, f fetcher, a *artifact) error {
	data, err := f.get(ctx, "https://raw.githubusercontent.com/alpinelinux/aports/master/"+al.path+"/APKBUILD")
	if err != nil {
		return err
	}
	m := pkgverRe.FindSubmatch(data)
	if m == nil || string(m[1]) != strings.TrimPrefix(a.version, "v") {
		return errNoCrossCheck
	}
	block := sha512blockRe.FindSubmatch(data)
	if block == nil {
		return fmt.Errorf("aports %s: no sha512sums", al.path)
	}
	sum, err := fileSHA512(a.path)
	if err != nil {
		return err
	}
	name := filepath.Base(a.url)
	if al.file != nil {
		name = al.file(a.version)
	}
	for _, line := range strings.Split(string(block[1]), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			if fields[0] != sum {
				return fmt.Errorf("sha512 differs from Alpine's for %s", name)
			}
			return nil
		}
	}
	return fmt.Errorf("aports %s: no sha512 for %s", al.path, name)
}

// freebsdDistinfo cross-checks the artifact against FreeBSD ports'
// distinfo for the same file (named file there, when given).
type freebsdDistinfo struct {
	port string // "net/bird2"
	file func(v string) string
}

func (d freebsdDistinfo) describe() string { return "FreeBSD ports' distinfo (" + d.port + ")" }

func (d freebsdDistinfo) verify(ctx context.Context, f fetcher, a *artifact) error {
	data, err := f.get(ctx, "https://raw.githubusercontent.com/freebsd/freebsd-ports/main/"+d.port+"/distinfo")
	if err != nil {
		return err
	}
	name := filepath.Base(a.url)
	if d.file != nil {
		name = d.file(a.version)
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		// SHA256 (bird-2.19.2.tar.gz) = aff89a...
		var file, sum string
		if n, _ := fmt.Sscanf(sc.Text(), "SHA256 (%s = %s", &file, &sum); n == 2 {
			file = strings.TrimSuffix(file, ")")
			if file == name || strings.HasSuffix(file, "/"+name) {
				if !strings.EqualFold(sum, a.sha256) {
					return fmt.Errorf("sha256 differs from FreeBSD ports' for %s", name)
				}
				return nil
			}
		}
	}
	return errNoCrossCheck
}

// gitTagFiles cross-checks a release tarball against GitHub's archive of
// the same version's git tag - another host than the tarball's: every
// file both have must be identical, and the tarball may carry no other
// file than those generated names - autotools' outputs, which the build
// deletes and regenerates, so what is compiled is the tag's own source.
type gitTagFiles struct {
	repo, tag string // tag as githubDigest's
	generated string // a file of shell case patterns, one per line
}

func (g gitTagFiles) describe() string {
	return "same files as the git tag on GitHub (" + g.repo + "), the rest regenerated by the build"
}

func (g gitTagFiles) verify(ctx context.Context, f fetcher, a *artifact) error {
	generated, err := readPatterns(g.generated)
	if err != nil {
		return err
	}
	archive, err := f.download(ctx, "https://github.com/"+g.repo+"/archive/refs/tags/"+releaseTag(g.tag, a.version)+".tar.gz")
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return errNoCrossCheck
	}
	if err != nil {
		return err
	}
	return sameAsTag(a.path, archive, generated)
}

// sameAsTag compares a release tarball with its tag's archive (both
// .tar.gz, each under its own top directory).
func sameAsTag(release, tag string, generated []*regexp.Regexp) error {
	rel, err := tarFiles(release)
	if err != nil {
		return err
	}
	tagged, err := tarFiles(tag)
	if err != nil {
		return err
	}
	common := 0
	var only []string
	for name, sum := range rel {
		if t, ok := tagged[name]; ok {
			if t != sum {
				return fmt.Errorf("%s differs from the tag's", name)
			}
			common++
			continue
		}
		if !slices.ContainsFunc(generated, func(re *regexp.Regexp) bool { return re.MatchString(name) }) {
			only = append(only, name)
		}
	}
	if len(only) > 0 {
		slices.Sort(only)
		return fmt.Errorf("not in the tag, and not a generated file: %s", strings.Join(only, ", "))
	}
	if common == 0 {
		return errors.New("no file in common with the tag")
	}
	return nil
}

// tarFiles lists a .tar.gz's files below its top directory, each by its
// content's sha256 - a link by its target's, as extracted: a tarball may
// store as a hard link what git has as a symlink, or the other way round.
// A symlink pointing out of the archive keeps its target's name.
func tarFiles(archive string) (map[string]string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(archive), err)
	}
	sums := map[string]string{}
	links := map[string]string{} // name -> the name it links to
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(archive), err)
		}
		_, name, _ := strings.Cut(hdr.Name, "/")
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader:
		case tar.TypeReg:
			h := sha256.New()
			if _, err := io.CopyN(h, tr, hdr.Size); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", filepath.Base(archive), name, err)
			}
			sums[name] = hex.EncodeToString(h.Sum(nil))
		case tar.TypeSymlink:
			if strings.HasPrefix(hdr.Linkname, "/") {
				sums[name] = "symlink " + hdr.Linkname
			} else {
				links[name] = path.Join(path.Dir(name), hdr.Linkname) //nolint:gosec // G305: a name to look up in this archive, nothing is extracted
			}
		case tar.TypeLink:
			_, target, _ := strings.Cut(hdr.Linkname, "/")
			links[name] = target
		default:
			return nil, fmt.Errorf("%s: %s: unexpected entry type %q", filepath.Base(archive), name, hdr.Typeflag)
		}
	}
	for name, target := range links {
		for range 8 {
			next, ok := links[target]
			if !ok {
				break
			}
			target = next
		}
		if sum, ok := sums[target]; ok {
			sums[name] = sum
		} else {
			sums[name] = "symlink " + target
		}
	}
	return sums, nil
}

// readPatterns reads a file of shell case patterns ('*' and '?' match
// '/' too), one per line, '#' starting a comment line.
func readPatterns(path string) ([]*regexp.Regexp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.ContainsAny(line, `[]\`) {
			return nil, fmt.Errorf("%s: %q: only '*' and '?' are understood", path, line)
		}
		re := regexp.QuoteMeta(line)
		re = strings.ReplaceAll(re, `\*`, ".*")
		re = strings.ReplaceAll(re, `\?`, ".")
		out = append(out, regexp.MustCompile("^"+re+"$"))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no pattern", path)
	}
	return out, nil
}

func fileSHA512(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha512.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
