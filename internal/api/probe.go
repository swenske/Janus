package api

import (
	"errors"
	"net/netip"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/probe"
)

// probeSlots: 4 probes at once per node - each a few sockets for up to a
// minute, not a way to load it.
var probeSlots = make(chan struct{}, 4)

// Probe tests the network from the node (internal/probe), each step
// streamed as it happens.
func (s *System) Probe(req *janusv1alpha1.ProbeRequest, stream janusv1alpha1.SystemService_ProbeServer) error {
	r := probe.Request{
		Kind: req.GetKind(), Target: req.GetTarget(), RecordType: req.GetRecordType(), Server: req.GetDnsServer(),
		ConnectTo: req.GetConnectTo(), SNI: req.GetSni(), ALPN: req.GetAlpn(), CAPEM: []byte(req.GetCaPem()), Method: req.GetMethod(),
		Count: int(req.GetCount()), Interval: time.Duration(req.GetIntervalMs()) * time.Millisecond, Size: int(req.GetSize()),
		DontFragment: req.GetDontFragment(), MaxHops: int(req.GetMaxHops()), Timeout: time.Duration(req.GetTimeoutMs()) * time.Millisecond,
	}
	if src := req.GetSource(); src != "" {
		a, err := netip.ParseAddr(src)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "source %q: an address", src)
		}
		r.Source = a
	}
	for _, h := range req.GetHeaders() {
		r.Headers = append(r.Headers, [2]string{h.GetName(), h.GetValue()})
	}
	select {
	case probeSlots <- struct{}{}:
		defer func() { <-probeSlots }()
	default:
		return status.Error(codes.ResourceExhausted, "4 probes are already running on this node: try again when one is done")
	}

	caller, _ := CallerFrom(stream.Context())
	events.Publish("probe.run", map[string]string{"kind": r.Kind, "target": r.Target, "by": caller.String()})
	err := probe.Run(stream.Context(), r, func(e probe.Event) error { return stream.Send(probeEvent(e)) })
	if errors.Is(err, probe.ErrInvalid) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}

func probeEvent(e probe.Event) *janusv1alpha1.ProbeEvent {
	ev := &janusv1alpha1.ProbeEvent{Stage: e.Stage, Ok: e.OK, ElapsedMs: e.ElapsedMS, Message: e.Message}
	switch {
	case e.DNS != nil:
		ev.Detail = &janusv1alpha1.ProbeEvent_Dns{Dns: &janusv1alpha1.ProbeDNS{Name: e.DNS.Name, Type: e.DNS.Type, Server: e.DNS.Server, Records: e.DNS.Records}}
	case e.Connect != nil:
		ev.Detail = &janusv1alpha1.ProbeEvent_Connect{Connect: &janusv1alpha1.ProbeConnect{Local: e.Connect.Local, Remote: e.Connect.Remote}}
	case e.TLS != nil:
		t := &janusv1alpha1.ProbeTLS{ServerName: e.TLS.ServerName, Version: e.TLS.Version, Cipher: e.TLS.Cipher, Alpn: e.TLS.ALPN, Verified: e.TLS.Verified, VerifyError: e.TLS.VerifyError, Ocsp: e.TLS.OCSP}
		for _, c := range e.TLS.Chain {
			t.Chain = append(t.Chain, &janusv1alpha1.ProbeCert{Subject: c.Subject, Issuer: c.Issuer, DnsNames: c.DNSNames, Ips: c.IPs, NotBeforeUnix: c.NotBefore.Unix(), NotAfterUnix: c.NotAfter.Unix(), Sha256: c.SHA256})
		}
		ev.Detail = &janusv1alpha1.ProbeEvent_Tls{Tls: t}
	case e.HTTP != nil:
		h := &janusv1alpha1.ProbeHTTP{Status: int32(e.HTTP.Status), StatusText: e.HTTP.StatusText, Proto: e.HTTP.Proto, BodyBytes: e.HTTP.BodyBytes, BodyTruncated: e.HTTP.BodyTruncated, BodyPreview: e.HTTP.BodyPreview, TtfbMs: e.HTTP.TTFBMS} //nolint:gosec // G115: an HTTP status
		for _, x := range e.HTTP.Headers {
			h.Headers = append(h.Headers, &janusv1alpha1.ProbeHeader{Name: x[0], Value: x[1]})
		}
		ev.Detail = &janusv1alpha1.ProbeEvent_Http{Http: h}
	case e.Reply != nil:
		r := e.Reply
		ev.Detail = &janusv1alpha1.ProbeEvent_Reply{Reply: &janusv1alpha1.ProbeReply{Seq: int32(r.Seq), From: r.From, Ttl: int32(r.TTL), Bytes: int32(r.Bytes), RttMs: r.RTTMS, Kind: r.Kind, Mtu: int32(r.MTU)}} //nolint:gosec // G115: a sequence up to 20, a TTL, an ICMP message's size, an MTU
	case e.Hop != nil:
		ev.Detail = &janusv1alpha1.ProbeEvent_Hop{Hop: &janusv1alpha1.ProbeHop{Ttl: int32(e.Hop.TTL), Address: e.Hop.Address, Name: e.Hop.Name, RttsMs: e.Hop.RTTsMS, Reached: e.Hop.Reached}} //nolint:gosec // G115: a TTL up to 30
	case e.Summary != nil:
		m := e.Summary
		ev.Detail = &janusv1alpha1.ProbeEvent_Summary{Summary: &janusv1alpha1.ProbeSummary{Sent: int32(m.Sent), Received: int32(m.Received), MinMs: m.MinMS, AvgMs: m.AvgMS, MaxMs: m.MaxMS, Reached: m.Reached, Hops: int32(m.Hops)}} //nolint:gosec // G115: counts up to 20, hops up to 30
	}
	return ev
}
