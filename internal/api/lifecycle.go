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
	"github.com/swenske/Janus/internal/releasetrust"
	"github.com/swenske/Janus/internal/schematic"
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
}

// Fixed partition sizes, matching image/disk/assemble.sh's own
// DATA_MB/HASH_MB - Upgrade enforces the same limits that script does
// at build time, rather than letting a write silently run past the end
// of a partition and corrupt whatever comes after it (the next
// partition, per image/disk/assemble.sh's fixed layout).
const (
	upgradeMaxDataBytes = 64 * 1024 * 1024
	upgradeMaxHashBytes = 4 * 1024 * 1024
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
		return upgradeMaxDataBytes
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
func fetchBundleFile(ctx context.Context, bundleRef, filename string) ([]byte, error) {
	if strings.HasPrefix(bundleRef, "http://") || strings.HasPrefix(bundleRef, "https://") {
		url := strings.TrimSuffix(bundleRef, "/") + "/" + filename
		fetchCtx, cancel := context.WithTimeout(ctx, upgradeFetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request for %s: %w", url, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(filepath.Join(bundleRef, filename))
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
		"this bundle is built from image schematic %s, but the node runs schematic %s: it would not keep the node's extensions. Use the update built for schematic %s (janus.sw-servers.net), or set allow_schematic_change to switch",
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
// the caller.
func scheduleReboot() {
	go func() {
		time.Sleep(replyGrace)
		syscall.Sync()
		if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
			log.Printf("lifecycle: reboot: %v", err)
		}
	}()
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
	bc, err := resolveBootContext()
	if err != nil {
		return nil, err
	}

	if err := espswitch.Activate(bc.espDevice, bc.targetSlot); err != nil {
		if errors.Is(err, espswitch.ErrUKINotStaged) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v - was image/disk/activate-slot.sh ever run for this disk?", err)
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	events.Publish("lifecycle.rollback", map[string]string{"from": bc.currentSlot, "to": bc.targetSlot})
	scheduleReboot()

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

	send := func(stage string, progress float64, message string) error {
		return stream.Send(&janusv1alpha1.UpgradeResponse{Stage: stage, Progress: progress, Message: message})
	}

	bc, err := resolveBootContext()
	if err != nil {
		return err
	}

	ctx := stream.Context()
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
	uki, err := fetchBundleFile(ctx, bundleRef, ukiName)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s: %v - does this bundle include a UKI for slot %s? (see image/release/assemble.sh)", ukiName, err, bc.targetSlot)
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

	squashfs, err := fetchBundleFile(ctx, bundleRef, "rootfs.squashfs")
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "rootfs.squashfs: %v", err)
	}
	if len(squashfs) > upgradeMaxDataBytes {
		return status.Errorf(codes.FailedPrecondition, "rootfs.squashfs is %d bytes, exceeds the %d-byte BOOT-*-DATA partition size", len(squashfs), upgradeMaxDataBytes)
	}
	if want := req.GetSource().GetSha256(); want != "" {
		if got := sha256Hex(squashfs); got != want {
			return status.Errorf(codes.FailedPrecondition, "rootfs.squashfs sha256 %s doesn't match requested %s", got, want)
		}
	}

	verity, err := fetchBundleFile(ctx, bundleRef, "rootfs.verity")
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "rootfs.verity: %v", err)
	}
	if len(verity) > upgradeMaxHashBytes {
		return status.Errorf(codes.FailedPrecondition, "rootfs.verity is %d bytes, exceeds the %d-byte BOOT-*-HASH partition size", len(verity), upgradeMaxHashBytes)
	}

	targetDataDev, _ := bootslot.SlotDataDevice(bc.disk, bc.targetSlot)
	targetHashDev, _ := bootslot.SlotHashDevice(bc.disk, bc.targetSlot)

	if err := send("writing-data", 0.3, fmt.Sprintf("writing rootfs.squashfs to slot %s (%s)", bc.targetSlot, targetDataDev)); err != nil {
		return err
	}
	if err := writePartitionFile(targetDataDev, squashfs); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", targetDataDev, err)
	}

	if err := send("writing-hash", 0.5, fmt.Sprintf("writing rootfs.verity to slot %s (%s)", bc.targetSlot, targetHashDev)); err != nil {
		return err
	}
	if err := writePartitionFile(targetHashDev, verity); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", targetHashDev, err)
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

	if err := send("rebooting", 1.0, fmt.Sprintf("rebooting into slot %s", bc.targetSlot)); err != nil {
		return err
	}

	events.Publish("lifecycle.upgrade", map[string]any{"from": bc.currentSlot, "to": bc.targetSlot, "source": req.GetSource().GetReference(), "wait_for_health": req.GetWaitForHealth(), "signature": signature})
	scheduleReboot()
	return nil
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
