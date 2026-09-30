package nodeproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// A browser download has no Ctrl-C to stop it cleanly, so every capture
// relayed here is bounded - the node enforces it (duration_seconds) and
// ends the stream itself once every packet is flushed.
const maxPcapDurationSeconds = 300

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func registerPcapRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("GET /api/pcap", func(w http.ResponseWriter, r *http.Request) {
		handlePcap(w, r, node)
	})
}

// handlePcap relays SystemService.PacketCapture as a .pcap file download:
// GET /api/pcap?interface=eth0&filter=...&duration=10[&snaplen=N][&promisc=true][&include_own_stream=true].
// The node leaves out its connection to this dashboard (the one carrying
// the capture) unless include_own_stream is set.
func handlePcap(w http.ResponseWriter, r *http.Request, node *store.Node) {
	// A GET, but not a harmless one: it starts a capture on the node,
	// promiscuous mode included - see sameOriginOrDirect.
	if !sameOriginOrDirect(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	iface := q.Get("interface")
	if iface == "" {
		http.Error(w, "interface is required", http.StatusBadRequest)
		return
	}
	duration, err := strconv.Atoi(q.Get("duration"))
	if err != nil || duration < 1 || duration > maxPcapDurationSeconds {
		http.Error(w, fmt.Sprintf("duration must be a number of seconds between 1 and %d", maxPcapDurationSeconds), http.StatusBadRequest)
		return
	}
	var snapLen uint64
	if s := q.Get("snaplen"); s != "" {
		if snapLen, err = strconv.ParseUint(s, 10, 32); err != nil {
			http.Error(w, "snaplen must be a number of bytes", http.StatusBadRequest)
			return
		}
	}

	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(duration)*time.Second+30*time.Second)
	defer cancel()
	stream, err := janusv1alpha1.NewSystemServiceClient(conn).PacketCapture(ctx, &janusv1alpha1.PacketCaptureRequest{
		Interface:        iface,
		BpfFilter:        q.Get("filter"),
		Promiscuous:      q.Get("promisc") == "true",
		SnapLen:          uint32(snapLen),
		DurationSeconds:  uint32(duration),
		IncludeOwnStream: q.Get("include_own_stream") == "true",
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("PacketCapture: %v", err), http.StatusBadGateway)
		return
	}

	// The node validates the interface and filter before sending
	// anything, so waiting for the first message (the pcap header, sent
	// within ~200ms) lets a bad request fail with a real HTTP error
	// instead of a download that turns out to be an error message.
	first, err := stream.Recv()
	if err != nil {
		http.Error(w, status.Convert(err).Message(), pcapHTTPStatus(err))
		return
	}

	filename := fmt.Sprintf("janus-%s-%s-%s.pcap",
		unsafeFilenameChars.ReplaceAllString(node.Name, "_"),
		unsafeFilenameChars.ReplaceAllString(iface, "_"),
		time.Now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)

	for msg := first; ; {
		if _, err := w.Write(msg.GetBytes()); err != nil {
			return // browser went away
		}
		if flusher != nil {
			flusher.Flush()
		}
		if msg, err = stream.Recv(); err != nil {
			// io.EOF is the node ending the capture after `duration`.
			// Anything else can only truncate the file now that the
			// headers are sent - logged, since the browser can't be told.
			if !errors.Is(err, io.EOF) {
				log.Printf("pcap relay for node %s: %v", node.ID, err)
			}
			return
		}
	}
}

func pcapHTTPStatus(err error) int {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.PermissionDenied, codes.Unauthenticated:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}
