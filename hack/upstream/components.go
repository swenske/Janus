package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// kind says where a component ends up - what a vulnerability in it means
// for Janus.
type kind string

const (
	kindNode       kind = "node"       // in every node image
	kindExtension  kind = "extension"  // in images built with that extension
	kindController kind = "controller" // in the Controller's image
	kindFirmware   kind = "firmware"   // in Raspberry Pi images
	kindBuild      kind = "build"      // builds something shipped, isn't shipped itself
	kindTest       kind = "test"       // only ever runs in tests
)

// track says which new upstream versions a component follows on its own;
// anything newer is shown, never proposed.
type track int

const (
	trackBranch track = iota // the pinned major.minor: "6.18.x", "3.4.x"
	trackMajor               // the pinned major: "2.x"
	trackAny                 // every new release
)

func (t track) String() string {
	switch t {
	case trackBranch:
		return "branch"
	case trackMajor:
		return "major"
	}
	return "any"
}

// component is one upstream pinned in versions.mk.
type component struct {
	name  string
	title string
	kind  kind
	// versionVar holds the pinned version; sumVars the sha256 of each
	// architecture's artifact ("" for a single, architecture-independent
	// one) - empty when nothing is downloaded (a Go module tag).
	versionVar string
	sumVars    map[string]string
	feed       feed
	track      track
	// prerelease: follow pre-releases too (an upstream that only makes
	// those).
	prerelease bool
	// url is the artifact downloaded for an architecture ("" when
	// sumVars has none).
	url func(v, arch string) string
	// checks must all pass for a bump; of crossChecks (independent
	// parties' checksums), at least one must, when there are some.
	checks      []check
	crossChecks []check
	// manual: never bumped by the tool - why.
	manual string
	// purl names it in the SBOM, when there's a better name than
	// pkg:generic/<name>@<version> (a Go module).
	purl  func(v string) string
	vulns []vulnSource
	// eol is the endoflife.date product, with the number of version
	// components naming a release cycle there.
	eol      string
	eolCycle int
	note     string
}

// Signing keys (hack/upstream/keys/<fingerprint>.asc).
var (
	keyGregKH     = "647F28654894E3BD457199BE38DBBDC86092693E"
	keyTorvalds   = "ABAF11C65A2970B130ABE3C479BE3E4300411886"
	keyAutosigner = "B8868C80BA62A1FFFAF5FDA9632D3A06589DA6B1"
	keyMadler     = "5ED46A6721D365587791E2AA783FCD8E58BCAFBA"
	keyNetfilter  = "8C5F7146A1757A65E2422A94D70D1A666ACF2B21"
	keyNetfilter0 = "37D964ACC04981C75500FB9BD55D978A8A1420E4"
	keyJansson    = "B5D6953E6D5059ED7ADA0F2FD3657D24D058434C"
	keyQEMU       = "CEACC9E15534EBABB82D3FA03353C9CEF108B584"
	keyHashiCorp  = "C874011F0AB405110D02105534365D9472D7468F"
	keyOpenTofu   = "E3E6E43D84CB852EADB0051D0C0AF313E5FD9F80"
)

func plain(v string) string { return strings.TrimPrefix(v, "v") }

func kernelDir(v string) string {
	return fmt.Sprintf("https://cdn.kernel.org/pub/linux/kernel/v%d.x/", mustVersion(v).nums[0])
}

func sameURL(suffix string) func(a *artifact) string {
	return func(a *artifact) string { return a.url + suffix }
}

// composeArch names an architecture the way Compose's release files do.
var composeArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// components is everything versions.mk pins, in its order.
var components = []*component{
	{
		name: "linux", title: "Linux kernel", kind: kindNode,
		versionVar: "KERNEL_VERSION", sumVars: map[string]string{"": "KERNEL_SHA256"},
		feed: kernelFeed{}, track: trackBranch,
		url: func(v, _ string) string { return kernelDir(v) + "linux-" + v + ".tar.xz" },
		checks: []check{
			gpgSig{sigURL: func(a *artifact) string { return strings.TrimSuffix(a.url, ".xz") + ".sign" },
				keys: []string{keyGregKH, keyTorvalds}, decompress: "xz"},
			signedSums{sumsURL: func(a *artifact) string { return kernelDir(a.version) + "sha256sums.asc" },
				keys: []string{keyAutosigner}},
		},
		vulns: []vulnSource{kernelCNA{}},
		eol:   "linux", eolCycle: 2,
		note: "the latest longterm branch",
	},
	{
		name: "haproxy", title: "HAProxy", kind: kindNode,
		versionVar: "HAPROXY_VERSION", sumVars: map[string]string{"": "HAPROXY_SHA256"},
		feed: haproxyFeed{}, track: trackBranch,
		url: func(v, _ string) string {
			return "https://www.haproxy.org/download/" + mustVersion(v).branch(2) + "/src/haproxy-" + v + ".tar.gz"
		},
		checks: []check{publishedSum{sumURL: sameURL(".sha256")}, haproxySum{}},
		vulns:  []vulnSource{haproxyBugs{}},
		eol:    "haproxy", eolCycle: 2,
		note: "the latest LTS branch",
	},
	{
		name: "musl-cross-make", title: "musl-cross-make (arm64 musl toolchain)", kind: kindBuild,
		versionVar: "MUSL_CROSS_MAKE_REF",
		feed:       githubCommit{repo: "richfelker/musl-cross-make", branch: "master"},
		manual:     "pinned by commit: the GCC/musl/binutils versions it embeds are noted in versions.mk by hand",
	},
	{
		name: "zlib", title: "zlib", kind: kindNode,
		versionVar: "ZLIB_VERSION", sumVars: map[string]string{"": "ZLIB_SHA256"},
		feed: githubReleases{repo: "madler/zlib"}, track: trackAny,
		url: func(v, _ string) string { return "https://zlib.net/fossils/zlib-" + plain(v) + ".tar.gz" },
		checks: []check{gpgSig{
			sigURL: func(a *artifact) string {
				v := plain(a.version)
				return "https://github.com/madler/zlib/releases/download/v" + v + "/zlib-" + v + ".tar.gz.asc"
			},
			keys: []string{keyMadler}}},
		crossChecks: []check{alpineAPKBUILD{path: "main/zlib"}},
		vulns: []vulnSource{osvGit{repo: "https://github.com/madler/zlib", tag: "v%s",
			// HAProxy, its only user here, calls deflateInit2/deflate/
			// deflateParams/deflateEnd: never the gz* file API, nor the
			// contrib/ programs (minizip, untgz...). OSV also maps here
			// other languages' wrappers that bundle zlib (Perl's).
			excluded: regexp.MustCompile(`\bgz(read|write|open|dopen|printf|puts|gets|fread|fwrite|seek|close|flush|getc|ungetc|buffer)\b|\bgzFile\b|` +
				`minizip|untgz|\bcontrib/|infback9|\bpuff\b|Compress::Raw::Zlib|\bfor Perl\b`),
			irrelevant: "HAProxy only uses zlib's in-memory deflate API"}},
	},
	{
		name: "aws-lc", title: "AWS-LC", kind: kindNode,
		versionVar: "AWSLC_VERSION", sumVars: map[string]string{"": "AWSLC_SHA256"},
		feed: githubReleases{repo: "aws/aws-lc"}, track: trackAny,
		url: func(v, _ string) string {
			return "https://github.com/aws/aws-lc/archive/refs/tags/v" + plain(v) + ".tar.gz"
		},
		// No signed artifact upstream: GitHub's tag archive, which Alpine and
		// FreeBSD pin too.
		crossChecks: []check{
			alpineAPKBUILD{path: "community/aws-lc", file: func(v string) string { return "aws-lc-" + plain(v) + ".tar.gz" }},
			freebsdDistinfo{port: "security/aws-lc", file: func(v string) string { return "aws-aws-lc-v" + plain(v) + "_GH0.tar.gz" }},
		},
		vulns: []vulnSource{ghsaRepo{repo: "aws/aws-lc", pkg: "AWS-LC"}},
	},
	{
		// Built from source with this tree's Go (extensions/prometheus-
		// node-exporter/Dockerfile): upstream's binary carries the Go it was
		// built with.
		name: "node-exporter", title: "Prometheus node_exporter", kind: kindExtension,
		versionVar: "NODE_EXPORTER_VERSION", sumVars: map[string]string{"": "NODE_EXPORTER_SHA256"},
		feed: githubReleases{repo: "prometheus/node_exporter"}, track: trackMajor,
		url:    goModuleZip("github.com/prometheus/node_exporter"),
		checks: []check{goSumDB{module: "github.com/prometheus/node_exporter"}},
		vulns:  []vulnSource{govulnSource{module: "github.com/prometheus/node_exporter"}},
		purl:   func(v string) string { return "pkg:golang/github.com/prometheus/node_exporter@v" + plain(v) },
	},
	{
		name: "libmnl", title: "libmnl (nftables extension)", kind: kindExtension,
		versionVar: "LIBMNL_VERSION", sumVars: map[string]string{"": "LIBMNL_SHA256"},
		feed: htmlIndex{url: "https://www.netfilter.org/pub/libmnl/", re: regexp.MustCompile(`libmnl-([0-9][0-9.]*[0-9])\.tar\.bz2`)}, track: trackAny,
		url:    func(v, _ string) string { return "https://www.netfilter.org/pub/libmnl/libmnl-" + v + ".tar.bz2" },
		checks: []check{gpgSig{sigURL: sameURL(".sig"), keys: []string{keyNetfilter, keyNetfilter0}}},
	},
	{
		name: "libnftnl", title: "libnftnl (nftables extension)", kind: kindExtension,
		versionVar: "LIBNFTNL_VERSION", sumVars: map[string]string{"": "LIBNFTNL_SHA256"},
		feed: htmlIndex{url: "https://www.netfilter.org/pub/libnftnl/", re: regexp.MustCompile(`libnftnl-([0-9][0-9.]*[0-9])\.tar\.xz`)}, track: trackAny,
		url:    func(v, _ string) string { return "https://www.netfilter.org/pub/libnftnl/libnftnl-" + v + ".tar.xz" },
		checks: []check{gpgSig{sigURL: sameURL(".sig"), keys: []string{keyNetfilter, keyNetfilter0}}},
	},
	{
		name: "nftables", title: "nftables (nftables extension)", kind: kindExtension,
		versionVar: "NFTABLES_VERSION", sumVars: map[string]string{"": "NFTABLES_SHA256"},
		feed: htmlIndex{url: "https://www.netfilter.org/pub/nftables/", re: regexp.MustCompile(`nftables-([0-9][0-9.]*[0-9])\.tar\.xz`)}, track: trackAny,
		url:    func(v, _ string) string { return "https://www.netfilter.org/pub/nftables/nftables-" + v + ".tar.xz" },
		checks: []check{gpgSig{sigURL: sameURL(".sig"), keys: []string{keyNetfilter, keyNetfilter0}}},
	},
	{
		name: "jansson", title: "Jansson (nftables extension)", kind: kindExtension,
		versionVar: "JANSSON_VERSION", sumVars: map[string]string{"": "JANSSON_SHA256"},
		feed: githubReleases{repo: "akheron/jansson"}, track: trackAny,
		url: func(v, _ string) string {
			v = plain(v)
			return "https://github.com/akheron/jansson/releases/download/v" + v + "/jansson-" + v + ".tar.gz"
		},
		checks: []check{gpgSig{sigURL: sameURL(".asc"), keys: []string{keyJansson}}},
		vulns:  []vulnSource{osvGit{repo: "https://github.com/akheron/jansson", tag: "v%s"}},
	},
	{
		name: "keepalived", title: "keepalived (VRRP extension)", kind: kindExtension,
		versionVar: "KEEPALIVED_VERSION", sumVars: map[string]string{"": "KEEPALIVED_SHA256"},
		feed: githubTags{repo: "acassen/keepalived"}, track: trackMajor,
		url: func(v, _ string) string {
			return "https://www.keepalived.org/software/keepalived-" + plain(v) + ".tar.gz"
		},
		// keepalived signs nothing.
		crossChecks: []check{alpineAPKBUILD{path: "community/keepalived"}},
		vulns:       []vulnSource{osvGit{repo: "https://github.com/acassen/keepalived", tag: "v%s"}},
	},
	{
		name: "qemu-guest-agent", title: "QEMU (qemu-ga, guest agent extension)", kind: kindExtension,
		versionVar: "QEMU_VERSION", sumVars: map[string]string{"": "QEMU_SHA256"},
		feed: htmlIndex{url: "https://download.qemu.org/", re: regexp.MustCompile(`qemu-([0-9]+\.[0-9]+\.[0-9]+)\.tar\.xz`)}, track: trackAny,
		url:    func(v, _ string) string { return "https://download.qemu.org/qemu-" + v + ".tar.xz" },
		checks: []check{gpgSig{sigURL: sameURL(".sig"), keys: []string{keyQEMU}}},
		vulns: []vulnSource{osvGit{repo: "https://github.com/qemu/qemu", tag: "v%s",
			// Only qemu-ga is built: the emulator's own devices aren't shipped.
			relevant: regexp.MustCompile(`(?i)guest[ -]agent|qemu-ga|\bqga\b`), irrelevant: "Janus ships only qemu-ga"}},
	},
	{
		name: "bird", title: "BIRD (BGP extension)", kind: kindExtension,
		versionVar: "BIRD_VERSION", sumVars: map[string]string{"": "BIRD_SHA256"},
		feed: htmlIndex{url: "https://bird.nic.cz/download/", re: regexp.MustCompile(`bird-([0-9]+\.[0-9]+\.[0-9]+)\.tar\.gz`)}, track: trackMajor,
		url: func(v, _ string) string { return "https://bird.nic.cz/download/bird-" + v + ".tar.gz" },
		// BIRD signs nothing.
		crossChecks: []check{freebsdDistinfo{port: "net/bird2"}},
		vulns:       []vulnSource{osvGit{repo: "https://gitlab.nic.cz/labs/bird", tag: "v%s"}},
		note:        "2.x: BIRD 3 aborts on janusd's protocol disabling",
	},
	{
		name: "rpi4-uefi", title: "Raspberry Pi 4 UEFI firmware (pftf/RPi4)", kind: kindFirmware,
		versionVar: "PFTF_RPI4_UEFI_VERSION", sumVars: map[string]string{"": "PFTF_RPI4_UEFI_SHA256"},
		feed: githubReleases{repo: "pftf/RPi4"}, track: trackAny,
		url: func(v, _ string) string {
			return "https://github.com/pftf/RPi4/releases/download/" + v + "/RPi4_UEFI_Firmware_" + v + ".zip"
		},
		checks: []check{githubDigest{repo: "pftf/RPi4", tag: "%s"}},
	},
	{
		name: "rpi5-uefi", title: "Raspberry Pi 5 UEFI firmware (rpi5-uefi)", kind: kindFirmware,
		versionVar: "RPI5_UEFI_VERSION", sumVars: map[string]string{"": "RPI5_UEFI_SHA256"},
		feed: githubReleases{repo: "NumberOneGit/rpi5-uefi"}, track: trackAny, prerelease: true,
		url: func(v, _ string) string {
			return "https://github.com/NumberOneGit/rpi5-uefi/releases/download/" + v + "/RPI5_D0.zip"
		},
		checks: []check{githubDigest{repo: "NumberOneGit/rpi5-uefi", tag: "%s"}},
	},
	{
		name: "docker-compose", title: "Docker Compose (Controller updater)", kind: kindController,
		versionVar: "DOCKER_COMPOSE_VERSION",
		sumVars:    map[string]string{"amd64": "DOCKER_COMPOSE_SHA256_AMD64", "arm64": "DOCKER_COMPOSE_SHA256_ARM64"},
		feed:       githubReleases{repo: "docker/compose"}, track: trackMajor,
		url: func(v, arch string) string {
			return "https://github.com/docker/compose/releases/download/" + v + "/docker-compose-linux-" + composeArch[arch]
		},
		checks: []check{publishedSum{sumURL: sameURL(".sha256")}, githubDigest{repo: "docker/compose", tag: "%s"}},
		vulns:  []vulnSource{govulnBinary{}},
	},
	{
		name: "opentofu", title: "OpenTofu (Terraform provider tests)", kind: kindTest,
		versionVar: "OPENTOFU_VERSION", sumVars: map[string]string{"amd64": "OPENTOFU_SHA256_AMD64"},
		feed: githubReleases{repo: "opentofu/opentofu"}, track: trackMajor,
		url: func(v, arch string) string {
			v = plain(v)
			return "https://github.com/opentofu/opentofu/releases/download/v" + v + "/tofu_" + v + "_linux_" + arch + ".tar.gz"
		},
		checks: []check{signedSums{
			sumsURL: func(a *artifact) string {
				v := plain(a.version)
				return "https://github.com/opentofu/opentofu/releases/download/v" + v + "/tofu_" + v + "_SHA256SUMS"
			},
			sigURL: func(a *artifact) string {
				v := plain(a.version)
				return "https://github.com/opentofu/opentofu/releases/download/v" + v + "/tofu_" + v + "_SHA256SUMS.gpgsig"
			},
			keys: []string{keyOpenTofu}}},
		eol: "opentofu", eolCycle: 2,
	},
	{
		name: "pebble", title: "Pebble (ACME tests)", kind: kindTest,
		versionVar: "PEBBLE_VERSION",
		feed:       githubReleases{repo: "letsencrypt/pebble"}, track: trackMajor,
		note: "go install'ed: Go's checksum database checks it",
	},
	{
		name: "consul", title: "Consul (Consul extension)", kind: kindExtension,
		versionVar: "CONSUL_VERSION",
		sumVars:    map[string]string{"amd64": "CONSUL_SHA256_amd64", "arm64": "CONSUL_SHA256_arm64"},
		feed:       hashicorpFeed{product: "consul"}, track: trackMajor,
		url: func(v, arch string) string {
			return "https://releases.hashicorp.com/consul/" + v + "/consul_" + v + "_linux_" + arch + ".zip"
		},
		checks: []check{signedSums{
			sumsURL: func(a *artifact) string {
				return "https://releases.hashicorp.com/consul/" + a.version + "/consul_" + a.version + "_SHA256SUMS"
			},
			sigURL: func(a *artifact) string {
				return "https://releases.hashicorp.com/consul/" + a.version + "/consul_" + a.version + "_SHA256SUMS.sig"
			},
			keys: []string{keyHashiCorp}}},
		vulns: []vulnSource{govulnBinary{member: func(string, string) string { return "consul" }}},
		eol:   "consul", eolCycle: 2,
	},
}

func findComponent(name string) *component {
	for _, c := range components {
		if c.name == name {
			return c
		}
	}
	return nil
}

// archs is a component's architectures, sorted, "" for a single artifact.
func (c *component) archs() []string {
	var out []string
	for arch := range c.sumVars {
		out = append(out, arch)
	}
	slices.Sort(out)
	return out
}
