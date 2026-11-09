package api

import (
	"bytes"
	"context"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bootcommit"
	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/espswitch"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/releasetrust"
	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/verity"
)

// Lifecycle implements janusv1alpha1.LifecycleServiceServer.
//
// Rollback and Upgrade share a core trick: the running node can never
// shell out to `ukify`/`sbsign` (no package manager, by design), so both
// just move already-built UKIs into place instead of ever assembling
// one. Rollback moves a UKI image/disk/activate-slot.sh already staged at
// build/install time (\JANUS\UKI-A.EFI / UKI-B.EFI); Upgrade moves one
// that arrived as part of a "release bundle" (image/release/assemble.sh) -
// which exists specifically because a genuinely *new* rootfs has a root
// hash nobody could have pre-staged at the original install's build time.
// Install (install.go) is the one that can't get away with just moving
// bytes into place throughout: it has no existing partition table to
// build on, so it lays out the whole disk itself in pure Go
// (internal/diskimage + github.com/diskfs/go-diskfs, since sgdisk/
// mtools/mkfs.ext4 don't exist on the target OS either) - but even it
// never builds a UKI, taking both slots' pre-built ones from the same
// kind of release bundle Upgrade reads from.
type Lifecycle struct {
	janusv1alpha1.UnimplementedLifecycleServiceServer

	// HAProxy, if set, is drained and soft-stopped before the reboot that
	// ends Rollback and Upgrade, as SystemService.Reboot does: the virtual
	// IPs and anycast routes leave this node first.
	HAProxy *haproxy.Manager
	// Version is janusd's: Upgrade says how a bundle compares to it.
	Version string
	// CheckHardware: this janusd runs a node (-manage-host) - Upgrade and
	// Install refuse a release whose kernel can't drive its disk or
	// network interfaces (checkHardware). Off on a host janusd only runs
	// on (tests), whose devices are no node's.
	CheckHardware bool
}

// Upgrade writes no more than the target partition holds (its real
// size: 64 MiB data slots on disks made before 128 MiB ones, see
// lifecycle_slots.go), rather than letting a write run past the end of a
// partition into the next one. The hash partition is 4 MiB on every
// layout.
const (
	upgradeMaxHashBytes = 4 * 1024 * 1024
	// maxSlotBytes caps an upload of rootfs.squashfs before the slot's
	// own size does (uploadIntoSlot).
	maxSlotBytes = 1 << 30
	// uki-a.efi/uki-b.efi have no size check in fetchBundleFile itself
	// (read once into memory and used immediately) - this one's for
	// UploadReleaseFile specifically, which writes straight to
	// persistent STATE storage without ever holding the whole file in
	// memory, so it needs its own cap to keep a buggy/malicious caller
	// from filling STATE's own fixed partition size (image/disk/
	// assemble.sh's STATE_MB) via an unbounded stream. Generous
	// compared to every UKI actually built by this project so far
	// (~9-10MiB).
	uploadMaxUKIBytes = 32 * 1024 * 1024

	// upgradeFetchTimeout bounds a single file's HTTPS download - long
	// enough for a real release bundle's own squashfs over a modest
	// link, short enough that a hung/stalled download doesn't leave
	// this streaming RPC (and its caller) blocked indefinitely.
	upgradeFetchTimeout = 5 * time.Minute
	// upgradeDataTimeout bounds the root filesystem's stream into the
	// slot - up to 128 MiB, where the other files are a few.
	upgradeDataTimeout = 10 * time.Minute
)

// releaseStagingDir is where UploadReleaseFile writes uploaded release-
// bundle files - a fixed subdirectory directly on the raw STATE mount
// (rootfs/init/main.go's mountState mounts STATE wholesale at
// /etc/.state; see its own doc comment for why - most of it is reached
// through narrower per-purpose bind mounts like /etc/janus/pki, but
// this project's own QEMU upgrade tests already established the
// convention of writing ad hoc release-bundle content directly under
// /etc/.state/<name> - see hack/qemu-lifecycle-upgrade-test.sh's own
// "upgrade/" directory). A var, not a const, so a test can point it
// elsewhere, same convention internal/bootcommit.Dir/internal/
// selfregister.Dir already use.
var releaseStagingDir = "/etc/.state/upgrade-incoming"

// releaseFileMaxBytes returns the byte cap for a given release-bundle
// filename, or 0 if the name isn't one of the fixed four this project's
// release bundles ever contain (image/release/assemble.sh's own
// output) - UploadReleaseFile refuses anything else outright, so a
// caller can never stage an arbitrary filename onto persistent storage.
func releaseFileMaxBytes(filename string) int64 {
	switch filename {
	case "rootfs.squashfs":
		return maxSlotBytes
	case "rootfs.verity":
		return upgradeMaxHashBytes
	case "uki-a.efi", "uki-b.efi":
		return uploadMaxUKIBytes
	default:
		return 0
	}
}

// fetchBundleFile reads one named file (e.g. "rootfs.squashfs") from a
// release bundle - bundleRef is either a local directory (the original,
// still-supported shape - image/release/assemble.sh's own output, or a
// controller-relayed UploadReleaseFile staging directory, see that
// RPC's own doc comment) or an "http://"/"https://" base URL, in which
// case the node fetches the file itself directly - completing the
// design ImageSource.reference's own proto comment already described
// ("OCI reference or HTTPS URL") but which Upgrade never implemented
// until now, local-path-only. TLS is verified against the system trust
// store (Go's own x509.SystemCertPool(), populated from /etc/ssl/certs/
// ca-certificates.crt - see ca-certificates/Dockerfile's own doc
// comment for why that bundle exists on this rootfs at all) - no
// pinning, unlike internal/nocloud's own seedfrom modes: a real release
// URL is always the public internet (GitHub Releases) with a
// well-known CA, not an operator-supplied arbitrary endpoint.
// errBundleFileMissing is a bundle answering that it has no such file -
// not a bundle that couldn't be reached.
var errBundleFileMissing = errors.New("not in the bundle")

func fetchBundleFile(ctx context.Context, bundleRef, filename string, maxBytes int64) ([]byte, error) {
	if strings.HasPrefix(bundleRef, "http://") || strings.HasPrefix(bundleRef, "https://") {
		url := strings.TrimSuffix(bundleRef, "/") + "/" + filename
		fetchCtx, cancel := context.WithTimeout(ctx, upgradeFetchTimeout)
		defer cancel()
		resp, err := getRetrying(fetchCtx, url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: %s: %w", url, resp.Status, errBundleFileMissing)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
		}
		return readCapped(resp.Body, filename, maxBytes)
	}
	f, err := os.Open(filepath.Join(bundleRef, filename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", err, errBundleFileMissing)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCapped(f, filename, maxBytes)
}

// readCapped reads a bundle file of at most maxBytes.
func readCapped(r io.Reader, filename string, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err == nil && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s is bigger than %s", filename, mib(uint64(maxBytes)))
	}
	return data, err
}

// verifyUKI is internal/releasetrust.VerifyUKI - a var so tests can
// substitute it.
var verifyUKI = releasetrust.VerifyUKI

// checkUKISignature refuses a UKI that isn't signed by a trusted release
// key (internal/releasetrust - which also explains why checking the UKI
// authenticates the whole bundle), unless src explicitly opts out for a
// development bundle. Returns a short description of what was decided,
// for progress messages and events.
func checkUKISignature(src *janusv1alpha1.ImageSource, name string, uki []byte) (string, error) {
	if src.GetInsecureSkipSignatureCheck() {
		log.Printf("lifecycle: %s: signature check skipped at the caller's request (insecure_skip_signature_check)", name)
		return "signature check skipped (insecure_skip_signature_check)", nil
	}
	if err := verifyUKI(uki); err != nil {
		return "", status.Errorf(codes.FailedPrecondition, "%s: %v - only bundles signed with a Janus release key are installed; for a development bundle, set insecure_skip_signature_check", name, err)
	}
	return "signature verified", nil
}

// ukiCmdline returns the kernel command line a UKI carries (its .cmdline
// PE section).
func ukiCmdline(uki []byte) (string, error) {
	f, err := pe.NewFile(bytes.NewReader(uki))
	if err != nil {
		return "", fmt.Errorf("not a PE image: %w", err)
	}
	defer f.Close()
	sec := f.Section(".cmdline")
	if sec == nil {
		return "", errors.New("no .cmdline section")
	}
	data, err := sec.Data()
	if err != nil {
		return "", fmt.Errorf("read .cmdline: %w", err)
	}
	return strings.TrimSpace(strings.TrimRight(string(data), "\x00")), nil
}

// checkSchematic refuses a UKI built from another image schematic than
// the node's (see internal/schematic): a node built with an extension
// must not lose it to an update built without it. src can allow the
// change explicitly. Returns what was decided, for progress messages.
func checkSchematic(src *janusv1alpha1.ImageSource, nodeCmdline string, uki []byte) (string, error) {
	cmdline, err := ukiCmdline(uki)
	if err != nil {
		return "", status.Errorf(codes.FailedPrecondition, "UKI: %v", err)
	}
	target, ok := schematic.FromCmdline(cmdline)
	if !ok {
		return "", status.Error(codes.FailedPrecondition, "the UKI's janus.schematic= isn't a valid schematic ID")
	}
	current, ok := schematic.FromCmdline(nodeCmdline)
	if !ok {
		return "", status.Error(codes.FailedPrecondition, "this node's own janus.schematic= isn't a valid schematic ID")
	}
	switch {
	case target == current:
		return "same image schematic (" + target[:12] + ")", nil
	case src.GetAllowSchematicChange():
		log.Printf("lifecycle: schematic change %s -> %s allowed by the caller", current[:12], target[:12])
		return fmt.Sprintf("image schematic changes from %s to %s (allowed)", current[:12], target[:12]), nil
	}
	return "", status.Errorf(codes.FailedPrecondition,
		"this bundle is built from image schematic %s, but the node runs schematic %s: it would not keep the node's extensions, HAProxy branch and kernel track. Use the update built for schematic %s (janus.sw-servers.net), or set allow_schematic_change to switch",
		target[:12], current[:12], current)
}

// bootContext is what both Rollback and Upgrade need to know about the
// disk they're running from: which slot is active, which slot they
// should target instead, and where the ESP is. Resolved once from
// /proc/cmdline via internal/bootslot - see mountState in
// rootfs/init/main.go for the same parsing applied to finding STATE
// instead.
type bootContext struct {
	disk        string
	currentSlot string
	targetSlot  string
	espDevice   string
}

func resolveBootContext() (*bootContext, error) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/cmdline: %v", err)
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "couldn't find a dm-mod.create= verity data device in /proc/cmdline - not booted from a recognized A/B image")
	}
	current, ok := bootslot.ActiveSlot(dataDev)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "root device %q isn't a recognized A/B slot (see image/disk/assemble.sh's fixed partition layout)", dataDev)
	}
	disk, ok := bootslot.Disk(dataDev)
	if !ok {
		return nil, status.Errorf(codes.Internal, "couldn't derive the disk from root device %q", dataDev)
	}
	espDev, ok := bootslot.ESPDevice(dataDev)
	if !ok {
		return nil, status.Errorf(codes.Internal, "couldn't derive the ESP device from root device %q", dataDev)
	}
	return &bootContext{
		disk:        disk,
		currentSlot: current,
		targetSlot:  bootslot.OtherSlot(current),
		espDevice:   espDev,
	}, nil
}

// scheduleReboot reboots shortly after the caller returns, not before -
// an RPC that rebooted immediately would kill the connection before its
// own response (or, for Upgrade, its last stream message) ever reached
// the caller. HAProxy is drained and soft-stopped first (powerdown.go).
func (l *Lifecycle) scheduleReboot(cmd int) {
	go powerDown("lifecycle", l.HAProxy, cmd)
}

// writePartitionFile writes data to a partition device path directly
// (e.g. "/dev/vda4") - the kernel already knows that device's offset
// on the underlying disk from the GPT table it parsed at boot
// (CONFIG_EFI_PARTITION), so this needs no offset math of its own, unlike
// image/disk/assemble.sh's build-time `dd seek=` (which operates on a
// plain disk image file with no partition-aware block devices at all).
//
// Synced and closed explicitly, errors included: a write error the
// block layer only reports at fsync/close time must fail the Upgrade
// before the ESP is switched to this slot, not be discarded by a
// deferred Close.
func writePartitionFile(device string, data []byte) error {
	f, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	return f.Close()
}

func (l *Lifecycle) Rollback(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.RollbackResponse, error) {
	unlock, err := lockSlots()
	if err != nil {
		return nil, err
	}
	defer unlock()
	bc, err := resolveBootContext()
	if err != nil {
		return nil, err
	}

	// The other slot must hold what its staged kernel image boots: an
	// update may have written part of it and given up, and a slot that
	// can't mount its root leaves nothing to come back from.
	if err := verifySlotHook(bc, bc.targetSlot); err != nil {
		if errors.Is(err, espswitch.ErrUKINotStaged) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v - was image/disk/activate-slot.sh ever run for this disk?", err)
		}
		return nil, status.Errorf(codes.FailedPrecondition, "slot %s doesn't hold what its kernel image boots - %v: an update to it failed or was refused part way. Rollback would leave the node unable to boot; update it again instead", bc.targetSlot, err)
	}

	if err := espswitch.Activate(bc.espDevice, bc.targetSlot); err != nil {
		if errors.Is(err, espswitch.ErrUKINotStaged) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v - was image/disk/activate-slot.sh ever run for this disk?", err)
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	events.Publish("lifecycle.rollback", map[string]string{"from": bc.currentSlot, "to": bc.targetSlot})
	l.scheduleReboot(syscall.LINUX_REBOOT_CMD_RESTART)

	return &janusv1alpha1.RollbackResponse{ActiveSlot: bc.targetSlot}, nil
}

// Upgrade writes a new rootfs to the currently-inactive A/B slot from a
// "release bundle" (image/release/assemble.sh's own output shape:
// rootfs.squashfs, rootfs.verity, uki-a.efi, uki-b.efi) -
// req.Source.Reference is either a local directory (the original shape -
// a path on this node's own filesystem, e.g. a bundle staged there by
// some other means for network topologies where this node can't dial
// out at all) or an "http://"/"https://" base URL, in which case this
// node fetches each file itself (fetchBundleFile, above) - completing
// the real HTTPS distribution the proto comment always described,
// feeding real GitHub Releases once those exist. No image-registry/OCI
// support. Whatever the source, the target slot's UKI must be signed by
// a trusted release key (checkUKISignature, internal/releasetrust) -
// checked first, before anything else is fetched or written. The actual upgrade mechanics
// this proves are unchanged either way: writing new content into the
// inactive slot without disturbing STATE or the currently-running slot,
// then switching and rebooting into it, the same way
// LifecycleService.Rollback does for a slot switch with no new content.
//
// wait_for_health, when true, writes a persistent "boot pending
// confirmation" marker to STATE (internal/bootcommit) before switching
// the ESP and rebooting - the *next* boot's own cmd/janusd checks
// it at startup and, in the background, polls HAProxy's own stats
// socket (internal/haproxy.Manager.ShowInfo) until it succeeds
// repeatedly in a row, confirming the marker (internal/
// bootcommit.Confirm) - real, application-level health, not just "the
// daemon process is still running". If that never happens within
// UpgradeRequest.health_timeout_seconds, it reverts back to the slot
// that was active before this Upgrade call and reboots
// (internal/bootrevert.To), autonomously: there is no live caller left
// by then to stream progress back to (the original Upgrade call's
// connection died with the first reboot), so neither the confirmation
// nor a possible revert is ever visible over gRPC, only in the node's
// own logs and the eventual slot it comes back up on. A second,
// independent safety net (rootfs/init's Supervisor.GiveUpAfter) covers
// the one failure mode cmd/janusd's own check can't: janusd
// crashing too fast, or too often, to ever get a chance to run that
// check at all.
func (l *Lifecycle) Upgrade(req *janusv1alpha1.UpgradeRequest, stream janusv1alpha1.LifecycleService_UpgradeServer) error {
	bundleRef := req.GetSource().GetReference()
	if bundleRef == "" {
		return status.Errorf(codes.InvalidArgument, "source.reference is required - a local release bundle directory or an http(s):// URL (see image/release/assemble.sh)")
	}

	stage, progress := "verifying", 0.0
	send := func(st string, p float64, message string) error {
		stage, progress = st, p
		return stream.Send(&janusv1alpha1.UpgradeResponse{Stage: st, Progress: p, Message: message})
	}

	unlock, err := lockSlots()
	if err != nil {
		return err
	}
	defer unlock()
	bc, err := resolveBootContext()
	if err != nil {
		return err
	}

	// A download asked again (GitHub answering 504) is said where the
	// update stands.
	ctx := withFetchNotice(stream.Context(), func(msg string) { _ = send(stage, progress, msg) })
	verb := "reading"
	if strings.HasPrefix(bundleRef, "http://") || strings.HasPrefix(bundleRef, "https://") {
		verb = "downloading"
	}
	if err := send("verifying", 0.1, fmt.Sprintf("%s release bundle at %s", verb, bundleRef)); err != nil {
		return err
	}

	// The UKI first: it's what authenticates the bundle (its signed
	// command line pins the rootfs's dm-verity root hash), so a bundle
	// that isn't trusted is refused before the rootfs is even downloaded.
	ukiName := fmt.Sprintf("uki-%s.efi", strings.ToLower(bc.targetSlot))
	uki, err := fetchBundleFile(ctx, bundleRef, ukiName, uploadMaxUKIBytes)
	switch {
	case errors.Is(err, errBundleFileMissing):
		return status.Errorf(codes.FailedPrecondition, "%s: %v - does this bundle include a UKI for slot %s? (see image/release/assemble.sh)", ukiName, err, bc.targetSlot)
	case err != nil && verb == "downloading":
		return status.Errorf(codes.FailedPrecondition, "%s: %v - if the node can't reach that server, have the Controller push the bundle to it (Update, \"The Controller pushes it\") or upload it (janusctl lifecycle upload-release)", ukiName, err)
	case err != nil:
		return status.Errorf(codes.FailedPrecondition, "%s: %v", ukiName, err)
	}
	signature, err := checkUKISignature(req.GetSource(), ukiName, uki)
	if err != nil {
		return err
	}
	if err := send("verifying", 0.2, fmt.Sprintf("%s: %s", ukiName, signature)); err != nil {
		return err
	}
	// The same schematic, so the node keeps its extensions: the UKI's
	// command line, now authenticated, names the one it was built from.
	nodeCmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return status.Errorf(codes.Internal, "read /proc/cmdline: %v", err)
	}
	schematicMsg, err := checkSchematic(req.GetSource(), string(nodeCmdline), uki)
	if err != nil {
		return err
	}
	if err := send("verifying", 0.22, fmt.Sprintf("%s: %s", ukiName, schematicMsg)); err != nil {
		return err
	}

	// What the UKI boots - its dm-verity table, authenticated with it:
	// the size of the root filesystem, and the root digest its hash tree
	// must end in.
	table, err := ukiVerityTable(ukiName, uki)
	if err != nil {
		return err
	}
	targetDataDev, _ := bootslot.SlotDataDevice(bc.disk, bc.targetSlot)
	targetHashDev, _ := bootslot.SlotHashDevice(bc.disk, bc.targetSlot)
	slotBytes, err := partitionSize(targetDataDev)
	if err != nil {
		return status.Errorf(codes.Internal, "size of %s: %v", targetDataDev, err)
	}
	if table.DataBytes() > uint64(slotBytes) {
		return slotTooSmall("this release's root filesystem", table.DataBytes(), uint64(slotBytes))
	}

	// The hash tree before the root filesystem: small, and checked
	// against the root digest on its own - then each block of the root
	// filesystem is checked against it before it's written.
	hashTree, err := fetchBundleFile(ctx, bundleRef, "rootfs.verity", upgradeMaxHashBytes)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "rootfs.verity: %v", err)
	}
	// What a refusal leaves in the target slot: nothing written, or - a
	// root filesystem uploaded into it - that upload, not the slot's
	// previous release.
	refused := refusedSuffix(stagedSlot(bundleRef, bc.targetSlot))
	tree, err := checkHashTree(table, ukiName, hashTree, refused)
	if err != nil {
		return err
	}
	if err := send("verifying", 0.25, fmt.Sprintf("rootfs.verity: the hash tree %s pins (root hash %x…)", ukiName, table.RootDigest[:6])); err != nil {
		return err
	}

	dataCtx, cancelData := context.WithTimeout(ctx, upgradeDataTimeout)
	defer cancelData()
	data, err := openBundleData(dataCtx, bundleRef, bc.targetSlot, targetDataDev)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "rootfs.squashfs: %v", err)
	}
	defer data.Close()
	if data.size >= 0 && uint64(data.size) != table.DataBytes() {
		return notTheUKIs("rootfs.squashfs", fmt.Errorf("%d bytes, %s's table covers %d", data.size, ukiName, table.DataBytes()), ukiName, refused)
	}

	// Streamed into the slot, each block checked first: a slot's worth
	// of data is never held in memory. A block refused once others were
	// written leaves the slot holding neither release - Rollback then
	// refuses it (verifySlot) until an update completes.
	writing := fmt.Sprintf("writing rootfs.squashfs to slot %s (%s), each block checked against the hash tree", bc.targetSlot, targetDataDev)
	if data.staged {
		writing = fmt.Sprintf("checking the rootfs.squashfs uploaded into slot %s (%s) against the hash tree", bc.targetSlot, targetDataDev)
	}
	if err := send("writing-data", 0.3, writing); err != nil {
		return err
	}
	slotWriting(bc.targetSlot)
	written, sum, err := writeSlotData(targetDataDev, table, tree, data)
	switch {
	case errors.Is(err, verity.ErrData) && written == 0 && !data.staged:
		slotWritten(bc.targetSlot, nil, nil, true) // as it was: read it again
		return notTheUKIs("rootfs.squashfs", err, ukiName, nothingWritten)
	case errors.Is(err, verity.ErrData) && data.staged:
		slotWritten(bc.targetSlot, nil, errors.New("holds an uploaded root filesystem its update refused"), false)
		return notTheUKIs("rootfs.squashfs", err, ukiName, refused)
	case errors.Is(err, verity.ErrData):
		slotWritten(bc.targetSlot, nil, errors.New("holds part of an update that was refused"), false)
		return notTheUKIs("rootfs.squashfs", err, ukiName, fmt.Sprintf(
			"; slot %s's data partition no longer holds its previous release (%s written): Rollback won't switch to it - update the node again with a sound bundle",
			bc.targetSlot, mib(uint64(written))))
	case err != nil:
		slotWritten(bc.targetSlot, nil, errors.New("holds part of an update that failed"), false)
		return status.Errorf(codes.Internal, "write %s: %v", targetDataDev, err)
	}
	if want := req.GetSource().GetSha256(); want != "" && sum != want {
		slotWritten(bc.targetSlot, nil, errors.New("holds a root filesystem whose update was refused"), false)
		return status.Errorf(codes.FailedPrecondition, "rootfs.squashfs sha256 %s doesn't match requested %s - it's the one %s pins, but not the one asked for: slot %s now holds it, and Rollback won't switch to it", sum, want, ukiName, bc.targetSlot)
	}
	// What the bundle is - its release: an older one than this node runs
	// is installed all the same (going back is allowed), but never
	// without saying so.
	image, imageErr := readSlotImage(targetDataDev)
	// Its kernel must drive this machine: a kernel without the boot
	// disk's driver never gets as far as init, so nothing on the node
	// could roll it back.
	hardware, err := l.checkHardware(req.GetSource(), image, bc.disk)
	if err != nil {
		slotWritten(bc.targetSlot, nil, errors.New("holds a release whose kernel can't drive this machine"), false)
		return status.Errorf(status.Code(err), "%s - slot %s now holds it, and Rollback won't switch to it", status.Convert(err).Message(), bc.targetSlot)
	}
	slotWritten(bc.targetSlot, image, imageErr, false)
	if err := send("verifying", 0.45, describeImage(image, imageErr, l.Version)); err != nil {
		return err
	}
	if hardware != "" {
		if err := send("verifying", 0.46, "rootfs.squashfs: "+hardware); err != nil {
			return err
		}
	}

	if err := send("writing-hash", 0.5, fmt.Sprintf("writing rootfs.verity to slot %s (%s)", bc.targetSlot, targetHashDev)); err != nil {
		return err
	}
	if err := writePartitionFile(targetHashDev, hashTree); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", targetHashDev, err)
	}
	if data.staged {
		if err := os.Remove(filepath.Join(releaseStagingDir, stagedMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("lifecycle: remove %s: %v", stagedMarker, err)
		}
	}
	syscall.Sync()

	// Written before the ESP switch below, not after: once the ESP
	// points at bc.targetSlot, that switch has to be protected by a
	// marker already being in place, or a crash in the narrow window
	// between the two would leave an unconfirmed slot active with
	// nothing watching it.
	if req.GetWaitForHealth() {
		if err := bootcommit.Write(&bootcommit.Marker{
			Slot:                 bc.targetSlot,
			RevertTo:             bc.currentSlot,
			TriesLeft:            1,
			HealthTimeoutSeconds: int(req.GetHealthTimeoutSeconds()),
		}); err != nil {
			return status.Errorf(codes.Internal, "write boot-commit marker: %v", err)
		}
	}

	if err := send("switching-slot", 0.8, fmt.Sprintf("switching ESP to slot %s", bc.targetSlot)); err != nil {
		return err
	}
	if err := espswitch.Mount(bc.espDevice); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	writeErr := func() error {
		stagedPath := filepath.Join(espswitch.Mountpoint, "JANUS", fmt.Sprintf("UKI-%s.EFI", bc.targetSlot))
		if err := os.WriteFile(stagedPath, uki, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", stagedPath, err)
		}
		activePath := filepath.Join(espswitch.Mountpoint, "EFI", "BOOT", espswitch.BootFilename)
		if err := os.WriteFile(activePath, uki, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", activePath, err)
		}
		return nil
	}()
	if err := espswitch.Unmount(); err != nil {
		log.Printf("lifecycle: unmount %s: %v", espswitch.Mountpoint, err)
	}
	if writeErr != nil {
		return status.Errorf(codes.Internal, "%v", writeErr)
	}
	syscall.Sync()

	// KEXEC: jump into the new UKI's kernel without the firmware
	// (kexec.go). The slot is written and switched by now, so a kernel
	// that can't be loaded only costs the firmware's time: say so and
	// reboot through it.
	how := "rebooting into slot " + bc.targetSlot
	if image != nil && image.Version != "" {
		how += " (Janus " + image.Version + ")"
	}
	kexecLoaded := false
	if req.GetRebootMode() == janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC {
		if err := loadKexec(uki); err != nil {
			log.Printf("lifecycle: kexec: %v - rebooting through the firmware", err)
			how += fmt.Sprintf(" through the firmware - kexec: %v", err)
		} else {
			kexecLoaded = true
			how += " through kexec, without the firmware"
		}
	}
	if err := send("rebooting", 1.0, how); err != nil {
		return err
	}

	// A struct, not a map: its fields in this order - what a one-line
	// summary of the event shows first.
	events.Publish("lifecycle.upgrade", struct {
		To            string `json:"to"`
		Version       string `json:"version,omitempty"`
		From          string `json:"from"`
		FromVersion   string `json:"from_version,omitempty"`
		Kexec         bool   `json:"kexec"`
		WaitForHealth bool   `json:"wait_for_health"`
		Source        string `json:"source"`
		Signature     string `json:"signature"`
	}{bc.targetSlot, imageVersion(image), bc.currentSlot, l.Version, kexecLoaded, req.GetWaitForHealth(), req.GetSource().GetReference(), signature})
	l.scheduleReboot(rebootCmd(kexecLoaded))
	return nil
}

// imageVersion is the release image holds, "" when it doesn't say.
func imageVersion(image *schematic.ImageInfo) string {
	if image == nil {
		return ""
	}
	return image.Version
}

// UploadReleaseFile is the controller-relay half of the two update
// modes this project supports side by side (the other, node-initiated
// one is fetchBundleFile's own http(s):// branch, above) - for network
// topologies where a node can't dial out to fetch a bundle itself at
// all (a real, user-raised constraint: some load-balancer deployments
// sit behind a network boundary that only ever permits inbound
// connections). The controller already dials *into* this node for
// every other RPC, so relaying a bundle's bytes needs no new direction
// of connection, just a way to carry more data than a single unary
// message comfortably holds - hence client-streaming rather than a
// plain unary RPC with a bytes field.
//
// Writes straight to releaseStagingDir, one call per file, refusing any
// filename outside the fixed release-bundle set (releaseFileMaxBytes)
// so a caller can never stage arbitrary content under an arbitrary
// name. Never touches an A/B slot or reboots anything itself - purely a
// file transfer; the caller still has to make a separate Upgrade call
// afterward with source.reference set to the returned staging_dir,
// reusing 100% of Upgrade's own already-verified writing/slot-switching/
// health-check logic unchanged.
func (l *Lifecycle) UploadReleaseFile(stream janusv1alpha1.LifecycleService_UploadReleaseFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receive first message: %v", err)
	}

	filename := first.GetFilename()
	maxBytes := releaseFileMaxBytes(filename)
	if maxBytes == 0 {
		return status.Errorf(codes.InvalidArgument, "invalid filename %q - must be one of rootfs.squashfs, rootfs.verity, uki-a.efi, uki-b.efi", filename)
	}
	unlock, err := lockSlots()
	if err != nil {
		return err
	}
	defer unlock()
	if filename == "rootfs.squashfs" {
		return l.uploadIntoSlot(first, stream)
	}
	if n := first.GetSize(); n > uint64(maxBytes) {
		return status.Errorf(codes.FailedPrecondition, "%s is %d bytes, over the %d-byte limit for this file", filename, n, maxBytes)
	}

	if err := os.MkdirAll(releaseStagingDir, 0o700); err != nil {
		return status.Errorf(codes.Internal, "mkdir %s: %v", releaseStagingDir, err)
	}
	destPath := filepath.Join(releaseStagingDir, filename)
	f, err := os.Create(destPath)
	if err != nil {
		return status.Errorf(codes.Internal, "create %s: %v", destPath, err)
	}
	defer f.Close()

	var written int64
	writeChunk := func(chunk []byte) error {
		if len(chunk) == 0 {
			return nil
		}
		written += int64(len(chunk))
		if written > maxBytes {
			return status.Errorf(codes.FailedPrecondition, "%s exceeds the %d-byte limit for this file", filename, maxBytes)
		}
		_, err := f.Write(chunk)
		return err
	}

	if err := writeChunk(first.GetChunk()); err != nil {
		os.Remove(destPath)
		return err
	}
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.Remove(destPath)
			return status.Errorf(codes.Internal, "receive %s: %v", filename, err)
		}
		if err := writeChunk(msg.GetChunk()); err != nil {
			os.Remove(destPath)
			return err
		}
	}

	if err := f.Sync(); err != nil {
		return status.Errorf(codes.Internal, "sync %s: %v", destPath, err)
	}

	events.Publish("lifecycle.release_file.uploaded", map[string]any{"filename": filename, "bytes": written})
	return stream.SendAndClose(&janusv1alpha1.UploadReleaseFileResponse{
		StagingDir:   releaseStagingDir,
		BytesWritten: uint64(written),
	})
}
