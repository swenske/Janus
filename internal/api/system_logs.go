package api

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
)

const followPollInterval = 250 * time.Millisecond

// Events streams the event log (internal/events): everything still held
// after since_id, then new events as they happen, until the client
// cancels. IDs restart from 1 when janusd restarts.
func (s *System) Events(req *janusv1alpha1.EventsRequest, stream janusv1alpha1.SystemService_EventsServer) error {
	ctx := stream.Context()
	next := req.GetSinceId()
	for {
		changed := events.Changed()
		evs, n := events.Since(next)
		next = n
		for _, e := range evs {
			if err := stream.Send(&janusv1alpha1.Event{Id: e.ID, UnixTimeNs: e.Time.UnixNano(), Type: e.Type, Payload: e.Payload}); err != nil {
				return nil
			}
		}
		if len(evs) > 0 {
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
	}
}

// Logs streams a managed service's captured output: janusd's own log, or
// HAProxy's stdout/stderr. Kept in memory (the last few thousand lines),
// not persisted.
func (s *System) Logs(req *janusv1alpha1.LogsRequest, stream janusv1alpha1.SystemService_LogsServer) error {
	buf, ok := s.ServiceLogs[req.GetId()]
	if !ok {
		return status.Errorf(codes.NotFound, "no logs for service %q (known: %s)", req.GetId(), strings.Join(s.logIDs(), ", "))
	}
	ctx := stream.Context()
	changed := buf.Changed()
	lines, next := buf.Last(int(req.GetTailLines()))
	for {
		if len(lines) > 0 {
			if err := stream.Send(&janusv1alpha1.Data{Bytes: []byte(strings.Join(lines, "\n") + "\n")}); err != nil {
				return nil
			}
		}
		if !req.GetFollow() {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
		changed = buf.Changed()
		lines, next = buf.Since(next)
	}
}

func (s *System) logIDs() []string {
	ids := make([]string, 0, len(s.ServiceLogs))
	for id := range s.ServiceLogs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Dmesg streams the kernel ring buffer from /dev/kmsg, formatted like
// dmesg(1): "[seconds.micros] message". Without follow it stops at the
// end of the buffer.
func (s *System) Dmesg(req *janusv1alpha1.DmesgRequest, stream janusv1alpha1.SystemService_DmesgServer) error {
	fd, err := unix.Open("/dev/kmsg", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return status.Errorf(codes.Internal, "open /dev/kmsg: %v", err)
	}
	defer unix.Close(fd)

	ctx := stream.Context()
	record := make([]byte, 8192) // each read returns exactly one record
	var batch []byte
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := stream.Send(&janusv1alpha1.Data{Bytes: batch})
		batch = nil
		return err
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := unix.Read(fd, record)
		switch {
		case err == nil:
			if line, ok := formatKmsgRecord(record[:n]); ok {
				batch = append(batch, line...)
			}
			if len(batch) >= 32*1024 {
				if flush() != nil {
					return nil
				}
			}
		case errors.Is(err, unix.EPIPE):
			// The reader fell behind and records were overwritten -
			// the next read continues from the oldest one left.
		case errors.Is(err, unix.EAGAIN):
			if flush() != nil {
				return nil
			}
			if !req.GetFollow() {
				return nil
			}
			time.Sleep(followPollInterval)
		case errors.Is(err, unix.EINTR):
		default:
			return status.Errorf(codes.Internal, "read /dev/kmsg: %v", err)
		}
	}
}

// formatKmsgRecord turns one /dev/kmsg record ("prio,seq,usec,flags;msg"
// followed by optional " KEY=value" continuation lines) into a
// dmesg-style line.
func formatKmsgRecord(rec []byte) (string, bool) {
	header, rest, ok := strings.Cut(string(rec), ";")
	if !ok {
		return "", false
	}
	msg, _, _ := strings.Cut(rest, "\n")
	f := strings.Split(header, ",")
	if len(f) < 3 {
		return "", false
	}
	usec, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("[%5d.%06d] %s\n", usec/1e6, usec%1e6, msg), true
}
