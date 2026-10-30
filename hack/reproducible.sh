# Sourced by the image scripts: what makes two builds of a commit the
# same bytes (#433 - a rerun of the publishing job rebuilt every asset,
# none the same).
#
# SOURCE_DATE_EPOCH is the commit's date unless already set (as
# hack/janusctl-deb.sh): mtools and xorriso take their times from it.
# uuid_of turns a seed into a UUID, for what mkfs and sgdisk would draw
# at random - the same image, the same identifiers, as every disk
# cloned from it already shares; nothing finds a partition by them
# (PARTLABEL does).
#
# With the kernel's and nft's own build dates (kernel/Dockerfile,
# extensions/nftables/Dockerfile) and the rootfs's (rootfs/layer-and-
# squash.sh), every release asset is the same twice - but the signed
# UKIs and the images and ISO carrying them: a Secure Boot signature
# holds the time it was made (sbsign, like every signer, adds it as a
# signed attribute).

: "${SOURCE_DATE_EPOCH:=$(git -C "$(dirname "${BASH_SOURCE[0]}")" log -1 --format=%ct 2>/dev/null || date +%s)}"
export SOURCE_DATE_EPOCH

uuid_of() {
  local h
  h="$(printf '%s' "$1" | sha256sum)"
  printf '%s-%s-%s-%s-%s\n' "${h:0:8}" "${h:8:4}" "${h:12:4}" "${h:16:4}" "${h:20:12}"
}

# gpt_ids SEED N prints sgdisk's options giving the disk and its
# partitions 1..N GUIDs made from SEED.
gpt_ids() {
  local i
  printf '%s\n' -U "$(uuid_of "$1 disk")"
  for ((i = 1; i <= $2; i++)); do
    printf '%s\n' -u "$i:$(uuid_of "$1 partition $i")"
  done
}
