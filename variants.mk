# The HAProxy branches and kernel tracks an image can be built with: a
# schematic picks one of each ("haproxy": "3.2", "kernel": "longterm" -
# docs/image-factory.md), unset is the default below. The upstream
# version each one is, and its sha256, are pinned in versions.mk.
#
# Read by the Makefile and by internal/variants (hack/extpack turns it
# into the release's schematic-catalog.json; hack/upstream follows each
# variant's pins): keep to "NAME := value" lines.

# HAProxy LTS branches, newest first - the first one is the default, "the
# latest LTS", which images that don't pin a branch move to with the
# release that adds it. Only branches that build with AWS-LC: HAProxy
# 3.0 and newer (2.8 has no USE_OPENSSL_AWSLC). Each branch has its pins
# in versions.mk (HAPROXY_<x>_<y>_VERSION, _SHA256) and its end of
# upstream support here (haproxy.org's branch table, endoflife.date):
# the Controller warns nodes on a branch about to lose it.
HAPROXY_BRANCHES := 3.4 3.2 3.0
HAPROXY_3_4_EOL  := 2031-04-01
HAPROXY_3_2_EOL  := 2030-04-01
HAPROXY_3_0_EOL  := 2029-04-01

# Kernel tracks: kernel.org's newest "longterm" and newest "stable"
# releases, each following its moniker from branch to branch by itself.
# Each has its pins in versions.mk (KERNEL_<TRACK>_VERSION, _SHA256),
# its config (kernel/configs/janus_<track>_defconfig) and the list of
# files its build reads (kernel/built-files-<track>-<arch>.txt). arm64
# images are only built with the default track.
KERNEL_TRACKS        := longterm
KERNEL_DEFAULT_TRACK := longterm

# Variants a release no longer offers, as component:name:last-release
# (the newest release with it), e.g. haproxy:2.8:v2027.06.01: an image
# built with one stays on it, and is told where its updates stopped.
VARIANTS_RETIRED :=
