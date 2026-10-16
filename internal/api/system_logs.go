package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/logvol"
)

const followPollInterval = 250 * time.Millisecond

// Events streams the event log (internal/events): everything still held
// after since_id, then new events as they happen, until the client
// cancels. On a node the log is kept on STATE: IDs go on across janusd
// restarts, reboots and updates.
func (s *System) Events(req *janusv1alpha1.EventsRequest, stream janusv1alpha1.SystemService_EventsServer) error {
	return streamLog(stream.Context(), nil, req.GetSinceId(), stream.Send)
}

// streamLog sends what l holds after since, then new events as they
// come, until ctx ends; l nil is the event log.
func streamLog(ctx context.Context, l *events.Log, since uint64, send func(*janusv1alpha1.Event) error) error {
	sinceFn, changedFn := events.Since, events.Changed
	if l != nil {
		sinceFn, changedFn = l.Since, l.Changed
	}
	next := since
	for {
		changed := changedFn()
		evs, n := sinceFn(next)
		next = n
		for _, e := range evs {
			if err := send(&janusv1alpha1.Event{Id: e.ID, UnixTimeNs: e.Time.UnixNano(), Type: e.Type, Payload: e.Payload}); err != nil {
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
// HAProxy's stdout/stderr. This boot's is kept in memory (the last few
// thousand lines); a previous boot's (boot < 0) comes from the LOGS
// partition.
func (s *System) Logs(req *janusv1alpha1.LogsRequest, stream janusv1alpha1.SystemService_LogsServer) error {
	if req.GetBoot() != 0 {
		if req.GetFollow() {
			return status.Error(codes.InvalidArgument, "follow is for this boot's logs only")
		}
		return s.sendPrevious(req.GetBoot(), req.GetId(), int(req.GetTailLines()), stream.Send)
	}
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

// LogBoots lists the boots the LOGS partition holds logs of.
func (s *System) LogBoots(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.LogBootsResponse, error) {
	v := s.LogVolume
	if v == nil {
		return &janusv1alpha1.LogBootsResponse{}, nil
	}
	st := v.Stats()
	resp := &janusv1alpha1.LogBootsResponse{
		Available: true, Device: s.LogDevice, SizeBytes: uint64(st.SizeBytes),
		RecordsWritten: st.Records, RecordsDropped: st.Dropped,
	}
	boots, offsets := bootOffsets(v)
	for i, b := range boots {
		resp.Boots = append(resp.Boots, &janusv1alpha1.LogBoot{
			Offset: offsets[i], Number: b.Number, BootId: uuidString(b.BootID),
			StartedUnixNs: b.Started.UnixNano(), Mebibytes: uint32(b.Segments),
		})
	}
	return resp, nil
}

// bootOffsets pairs the boots v holds, newest first, with their offsets:
// 0 for this boot, -1 for the one before...
func bootOffsets(v *logvol.Volume) ([]logvol.Boot, []int32) {
	boots := v.Boots()
	shift := 0
	if len(boots) > 0 && boots[0].Number != v.Current() {
		shift = 1 // this boot wrote nothing yet: the newest held is -1
	}
	offsets := make([]int32, len(boots))
	for i := range boots {
		offsets[i] = int32(-(i + shift))
	}
	return boots, offsets
}

// bootNumber resolves an offset (-1: the boot before this one...) to the
// volume's number of that boot.
func (s *System) bootNumber(offset int32) (uint32, error) {
	v := s.LogVolume
	if v == nil {
		return 0, status.Error(codes.FailedPrecondition, "this node has no LOGS partition: only this boot's logs are kept, in memory")
	}
	if offset > 0 {
		return 0, status.Errorf(codes.InvalidArgument, "boot %d: 0 is this boot, -1 the one before...", offset)
	}
	boots, offsets := bootOffsets(v)
	for i, b := range boots {
		if offsets[i] == offset {
			return b.Number, nil
		}
	}
	return 0, status.Errorf(codes.NotFound, "the LOGS partition holds no boot %d (janusctl system boots)", offset)
}

// sendPrevious sends what service printed during a previous boot: its
// last tail lines, or everything when tail <= 0.
func (s *System) sendPrevious(offset int32, service string, tail int, send func(*janusv1alpha1.Data) error) error {
	boot, err := s.bootNumber(offset)
	if err != nil {
		return err
	}
	keep := func(r logvol.Record) bool { return r.Service == service }
	var batch []byte
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := send(&janusv1alpha1.Data{Bytes: batch})
		batch = nil
		return err
	}
	add := func(r logvol.Record) bool {
		batch = append(batch, r.Message...)
		batch = append(batch, '\n')
		return len(batch) < 32<<10 || flush() == nil
	}
	if tail > 0 {
		recs, err := s.LogVolume.Tail(boot, tail, keep)
		if err != nil {
			return status.Errorf(codes.Internal, "read %s: %v", s.LogDevice, err)
		}
		for _, r := range recs {
			if !add(r) {
				return nil
			}
		}
		return flush()
	}
	if err := s.LogVolume.Read(boot, func(r logvol.Record) bool { return !keep(r) || add(r) }); err != nil {
		return status.Errorf(codes.Internal, "read %s: %v", s.LogDevice, err)
	}
	return flush()
}

func uuidString(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Dmesg streams the kernel ring buffer from /dev/kmsg, formatted like
// dmesg(1): "[seconds.micros] message". Without follow it stops at the
// end of the buffer. A previous boot's (boot < 0) comes from the LOGS
// partition.
func (s *System) Dmesg(req *janusv1alpha1.DmesgRequest, stream janusv1alpha1.SystemService_DmesgServer) error {
	if req.GetBoot() != 0 {
		if req.GetFollow() {
			return status.Error(codes.InvalidArgument, "follow is for this boot's kernel messages only")
		}
		return s.sendPrevious(req.GetBoot(), "kernel", 0, stream.Send)
	}
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
	k, ok := logvol.ParseKmsg(rec)
	if !ok {
		return "", false
	}
	return k.Line() + "\n", true
}
