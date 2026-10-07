package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/consoledrain"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/shutdown"
)

// stateMountpoint is where rootfs/init mounts the persistent STATE
// partition (see its mountState) - a var so tests can point it elsewhere.
var stateMountpoint = "/etc/.state"

// replyGrace lets a response reach the caller before the action that
// cuts the connection happens.
const replyGrace = 2 * time.Second

// Reboot restarts the machine: HAProxy is soft-stopped first so
// in-flight connections get a chance to finish. DEFAULT and POWERCYCLE
// are a full firmware reboot; KEXEC jumps into the active UKI's kernel
// without the firmware (kexec.go) and is refused, nothing rebooted,
// when that can't be done.
func (s *System) Reboot(_ context.Context, req *janusv1alpha1.RebootRequest) (*janusv1alpha1.RebootResponse, error) {
	cmd := syscall.LINUX_REBOOT_CMD_RESTART
	if req.GetMode() == janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC {
		if err := loadKexecActive(); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "kexec: %v - reboot through the firmware instead", err)
		}
		cmd = syscall.LINUX_REBOOT_CMD_KEXEC
	}
	events.Publish("system.reboot", map[string]string{"mode": req.GetMode().String()})
	s.schedulePower(cmd)
	return &janusv1alpha1.RebootResponse{}, nil
}

// Shutdown powers the machine off, after the same graceful HAProxy stop.
func (s *System) Shutdown(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ShutdownResponse, error) {
	events.Publish("system.shutdown", nil)
	s.schedulePower(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	return &janusv1alpha1.ShutdownResponse{}, nil
}

// Restart exits janusd so rootfs/init's supervisor starts it again. HAProxy
// isn't interrupted: it keeps running meanwhile, and the new janusd takes
// it over with a seamless reload (-sf) on startup.
func (s *System) Restart(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.RestartResponse, error) {
	if os.Getpid() == 1 {
		return nil, status.Error(codes.FailedPrecondition, "janusd is PID 1 here (e.g. a container): nothing would start it again")
	}
	events.Publish("system.restart", nil)
	go func() {
		time.Sleep(replyGrace)
		log.Printf("janusd: exiting for a restart requested over the API")
		os.Exit(0)
	}()
	return &janusv1alpha1.RestartResponse{}, nil
}

// Reset returns the node to its just-installed state: wipe_state empties
// the persistent STATE partition - PKI (a new CA and admin certificate
// are generated and printed on the console at the next boot), applied
// HAProxy config, Controller registration, pending boot confirmations -
// then reboots. The A/B slots and the ESP aren't touched.
// wipe_ephemeral alone just reboots: everything ephemeral is tmpfs.
func (s *System) Reset(_ context.Context, req *janusv1alpha1.ResetRequest) (*janusv1alpha1.ResetResponse, error) {
	if !req.GetWipeState() && !req.GetWipeEphemeral() {
		return nil, status.Error(codes.InvalidArgument, "nothing to reset: set wipe_state and/or wipe_ephemeral")
	}
	if req.GetWipeState() {
		mounted, err := isMountpoint(stateMountpoint)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check %s: %v", stateMountpoint, err)
		}
		if !mounted {
			return nil, status.Errorf(codes.FailedPrecondition, "%s isn't a mounted STATE partition on this node - nothing persistent to wipe", stateMountpoint)
		}
		shutdown.Run() // what's saved goes before the wipe, never after
		if err := wipeDirContents(stateMountpoint); err != nil {
			return nil, status.Errorf(codes.Internal, "wipe %s: %v", stateMountpoint, err)
		}
	}
	events.Publish("system.reset", map[string]bool{"wipe_state": req.GetWipeState(), "wipe_ephemeral": req.GetWipeEphemeral()})
	s.schedulePower(syscall.LINUX_REBOOT_CMD_RESTART)
	return &janusv1alpha1.ResetResponse{}, nil
}

func (s *System) schedulePower(cmd int) {
	go func() {
		time.Sleep(replyGrace)
		if s.HAProxy != nil {
			if err := s.HAProxy.Stop(5 * time.Second); err != nil {
				log.Printf("system: stop haproxy: %v", err)
			}
		}
		shutdown.Run()
		syscall.Sync()
		consoledrain.Wait(os.Stderr, 2*time.Second)
		if err := syscall.Reboot(cmd); err != nil {
			log.Printf("system: reboot(%#x): %v", cmd, err)
		}
	}()
}

// isMountpoint reports whether path is on a different filesystem than
// its parent.
func isMountpoint(path string) (bool, error) {
	var self, parent syscall.Stat_t
	if err := syscall.Stat(path, &self); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return self.Dev != parent.Dev, nil
}

// wipeDirContents removes everything under dir except ext4's lost+found.
// A subdirectory that is also bind-mounted elsewhere (pki/, haproxy/,
// boot/, controller/ - see rootfs/init's mountState) is emptied even if
// the directory itself can't be removed while mounted.
func wipeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "lost+found" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		children, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := os.RemoveAll(filepath.Join(path, c.Name())); err != nil {
				return fmt.Errorf("remove %s: %w", filepath.Join(path, c.Name()), err)
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, syscall.EBUSY) {
			return err
		}
	}
	return nil
}
