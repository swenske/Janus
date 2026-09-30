// Package timesync keeps the node's clock on time without an NTP daemon:
// janusd queries the NTP servers itself (SNTP, github.com/beevik/ntp)
// and disciplines the kernel clock through adjtimex(2) - a step for a
// large error, the kernel's own phase-locked loop for the rest. Marking
// the clock synchronized also lets the kernel copy system time to the
// hardware clock every 11 minutes on x86 (CONFIG_GENERIC_CMOS_UPDATE).
//
// The servers come from the node's network configuration: configured
// ones, else those the boot DHCP lease gave, else pool.ntp.org (see
// internal/netconfig.EffectiveNTP).
//
// SNTP isn't authenticated; NTS would be, at a much larger cost.
package timesync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/beevik/ntp"
	"golang.org/x/sys/unix"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

const (
	// stepThreshold: a larger error is corrected at once, a smaller one
	// slewed (ntpd's own 128 ms).
	stepThreshold = 128 * time.Millisecond
	minPoll       = 64 * time.Second
	maxPoll       = 1024 * time.Second
	// Until the first success, retry quickly: a node's first boot waits
	// on it before generating its certificates.
	unsyncedRetry = 10 * time.Second
	syncedRetry   = time.Minute
	queryTimeout  = 5 * time.Second
)

// Options configures a Service.
type Options struct {
	// Manage enables adjusting the clock. Off (janusd on a developer's
	// machine, or in a container), the Service only reports.
	Manage bool
	// Servers returns the servers to use ("host" or "host:port") and
	// where they came from.
	Servers func() (servers []string, source string)

	// For tests.
	query  func(addr string) (*ntp.Response, error)
	adjust func(offset, rtt time.Duration) error
}

// Service is the node's clock synchronization.
type Service struct {
	opts Options
	kick chan struct{}

	mu         sync.Mutex
	synced     chan struct{} // closed at the first successful exchange
	syncedOnce bool
	lastServer string
	lastSync   time.Time
	offset     time.Duration
	stratum    uint8
	lastErr    string
}

func New(opts Options) *Service {
	if opts.query == nil {
		opts.query = func(addr string) (*ntp.Response, error) {
			return ntp.QueryWithOptions(addr, ntp.QueryOptions{Timeout: queryTimeout})
		}
	}
	if opts.adjust == nil {
		opts.adjust = adjustClock
	}
	return &Service{opts: opts, kick: make(chan struct{}, 1), synced: make(chan struct{})}
}

// Kick makes the Service query again soon - the servers changed.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// WaitSynced waits for the first successful exchange, up to timeout.
func (s *Service) WaitSynced(timeout time.Duration) bool {
	select {
	case <-s.synced:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Run synchronizes until ctx ends.
func (s *Service) Run(ctx context.Context) {
	poll := minPoll
	for {
		wait := poll
		if err := s.syncOnce(); err != nil {
			s.mu.Lock()
			wait = syncedRetry
			if !s.syncedOnce {
				wait = unsyncedRetry
			}
			s.mu.Unlock()
			poll = minPoll
		} else {
			poll = min(poll*2, maxPoll)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			poll = minPoll
		case <-time.After(wait):
		}
	}
}

// syncOnce queries the servers in order until one answers validly, and
// corrects the clock with its answer.
func (s *Service) syncOnce() error {
	servers, _ := s.opts.Servers()
	var errs []error
	for _, server := range servers {
		resp, err := s.opts.query(server)
		if err == nil {
			err = resp.Validate()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", server, err))
			continue
		}
		if s.opts.Manage {
			if err := s.opts.adjust(resp.ClockOffset, resp.RTT); err != nil {
				errs = append(errs, fmt.Errorf("adjust the clock: %w", err))
				break
			}
		}
		s.mu.Lock()
		first := !s.syncedOnce
		s.syncedOnce = true
		s.lastServer, s.lastSync, s.offset, s.stratum, s.lastErr = server, time.Now(), resp.ClockOffset, resp.Stratum, ""
		s.mu.Unlock()
		if first {
			close(s.synced)
			log.Printf("timesync: synchronized with %s (offset %s)", server, resp.ClockOffset)
		}
		return nil
	}
	if len(servers) == 0 {
		errs = append(errs, errors.New("no NTP server"))
	}
	err := errors.Join(errs...)
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
	return err
}

// Status reports the synchronization. "synchronized" is the kernel's own
// view (adjtimex), not just this Service's last success.
func (s *Service) Status() *janusv1alpha1.TimeStatus {
	servers, source := s.opts.Servers()
	s.mu.Lock()
	st := &janusv1alpha1.TimeStatus{
		Servers: servers, Source: source, LastServer: s.lastServer,
		OffsetNs: int64(s.offset), Stratum: uint32(s.stratum), Error: s.lastErr,
	}
	if !s.lastSync.IsZero() {
		st.LastSyncUnix = s.lastSync.Unix()
	}
	s.mu.Unlock()
	var tx unix.Timex
	if state, err := unix.Adjtimex(&tx); err == nil {
		st.Synchronized = state != unix.TIME_ERROR && tx.Status&unix.STA_UNSYNC == 0
	}
	return st
}

// adjustClock corrects the kernel clock by offset (server minus local):
// a step if it's large, otherwise the kernel's PLL slews it. Either way
// the clock is then marked synchronized, with the round trip as its
// error bound.
func adjustClock(offset, rtt time.Duration) error {
	slew := offset
	if offset >= stepThreshold || offset <= -stepThreshold {
		tx := unix.Timex{Modes: unix.ADJ_SETOFFSET | unix.ADJ_NANO}
		sec := int64(offset / time.Second)
		nsec := int64(offset % time.Second)
		if nsec < 0 { // the kernel wants 0 <= nsec < 1e9
			sec--
			nsec += int64(time.Second)
		}
		tx.Time.Sec, tx.Time.Usec = sec, nsec // Usec holds nanoseconds with ADJ_NANO
		if _, err := unix.Adjtimex(&tx); err != nil {
			return fmt.Errorf("step by %s: %w", offset, err)
		}
		log.Printf("timesync: clock stepped by %s", offset)
		slew = 0
	}
	errUS := max(rtt/2, time.Millisecond).Microseconds()
	tx := unix.Timex{
		Modes:    unix.ADJ_OFFSET | unix.ADJ_STATUS | unix.ADJ_NANO | unix.ADJ_MAXERROR | unix.ADJ_ESTERROR,
		Offset:   int64(slew),
		Status:   unix.STA_PLL | unix.STA_NANO, // STA_UNSYNC cleared
		Maxerror: errUS,
		Esterror: errUS,
	}
	if _, err := unix.Adjtimex(&tx); err != nil {
		return fmt.Errorf("discipline: %w", err)
	}
	return nil
}
