package api

import (
	"errors"
	"io"
	"log"
	"os"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/statexfer"
)

// A node's state carried to another (internal/statexfer,
// docs/guide/replacing-a-node.md). It holds the node's private keys - its
// certificates', its ACME account's, its CA's when the identity goes -:
// it never goes through the Controller, whose relay would see it.

// stateTransferAllowed refuses what can't carry a node's state: a call
// the Controller relays, a janusd without STATE.
func (s *System) stateTransferAllowed(c Caller) error {
	if c.Via != "" {
		return status.Errorf(codes.PermissionDenied, "a node's state carries its private keys: it never goes through %s - call the node itself (janusctl with its own or a fleet certificate)", c.Via)
	}
	if s.StateRoot == "" {
		return status.Error(codes.FailedPrecondition, "this janusd keeps no STATE: it isn't a node")
	}
	return nil
}

// StateExport streams the node's state.
func (s *System) StateExport(req *janusv1alpha1.StateExportRequest, stream janusv1alpha1.SystemService_StateExportServer) error {
	caller, _ := CallerFrom(stream.Context())
	if err := s.stateTransferAllowed(caller); err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	w := &dataWriter{send: func(b []byte) error { return stream.Send(&janusv1alpha1.Data{Bytes: b}) }}
	m, err := statexfer.Export(s.StateRoot, statexfer.ExportOptions{Identity: req.GetIncludeIdentity(), Hostname: hostname, Version: s.BuildVersion, Schematic: CurrentSchematic()}, w)
	if errors.Is(err, statexfer.ErrRotating) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return status.Errorf(codes.Internal, "export the state: %v", err)
	}
	if err := w.flush(); err != nil {
		return err
	}
	log.Printf("state: exported (%d files, identity %v) - by %s", len(m.Files), m.Identity, caller)
	events.Publish("system.state_exported", map[string]any{"files": len(m.Files), "identity": m.Identity, "by": caller.String()})
	return nil
}

// StateImport stages another node's state, for janusd's next start.
func (s *System) StateImport(stream janusv1alpha1.SystemService_StateImportServer) error {
	caller, _ := CallerFrom(stream.Context())
	if err := s.stateTransferAllowed(caller); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "no state: %v", err)
	}
	opts := first.GetOptions()
	if opts == nil {
		return status.Error(codes.InvalidArgument, "the first message carries the options")
	}
	if why := statexfer.InUse(s.StateRoot); why != "" {
		return status.Errorf(codes.FailedPrecondition, "this node is in use (%s): Reset it first (janusctl system reset -state), then import", why)
	}
	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write(first.GetData()); err != nil {
			return
		}
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				return // Stage stopped reading
			}
		}
	}()
	st, err := statexfer.Stage(s.StateRoot, statexfer.ImportOptions{Identity: opts.GetReuseIdentity(), ApplyNetwork: opts.GetApplyNetwork()}, pr)
	pr.CloseWithError(errors.New("the import is over"))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "the state: %v", err)
	}
	m := st.Manifest
	resp := &janusv1alpha1.StateImportResponse{
		SourceHostname: m.Hostname, SourceVersion: m.Version, SourceSchematic: m.Schematic,
		Files: st.Files, Skipped: st.Skipped, HeldNetworkConfig: st.HeldNetwork,
		IdentityReused: opts.GetReuseIdentity(), Rebooting: !opts.GetNoReboot(),
	}
	if cur := CurrentSchematic(); m.Schematic != "" && cur != "" && cur != m.Schematic {
		resp.Warnings = append(resp.Warnings, "the exported node ran another image ("+m.Schematic+", this one "+cur+"): what an extension this image lacks was configured for stays unused")
	}
	if m.Version != s.BuildVersion {
		resp.Warnings = append(resp.Warnings, "the exported node ran "+m.Version+", this one "+s.BuildVersion)
	}
	log.Printf("state: %s's state staged (%d files, identity %v) - in place at janusd's next start - by %s", m.Hostname, len(st.Files), opts.GetReuseIdentity(), caller)
	events.Publish("system.state_imported", map[string]any{"from": m.Hostname, "files": len(st.Files), "identity": opts.GetReuseIdentity(), "by": caller.String()})
	if err := stream.SendAndClose(resp); err != nil {
		return err
	}
	if resp.Rebooting {
		s.schedulePower(syscall.LINUX_REBOOT_CMD_RESTART)
	}
	return nil
}
