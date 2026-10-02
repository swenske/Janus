package nodeproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"
)

// Shutdown asks node to power off cleanly (it stops HAProxy first) -
// before the Controller destroys the virtual machine it runs in.
func Shutdown(ctx context.Context, node *store.Node) error {
	conn, err := dialNode(node)
	if err != nil {
		return err
	}
	_, err = janusv1alpha1.NewSystemServiceClient(conn).Shutdown(ctx, &emptypb.Empty{})
	return err
}

// SSE is the per-node pages' Server-Sent Events writer (heartbeat
// included), for dashboardd's own streams.
type SSE struct{ s *sseWriter }

func NewSSE(w http.ResponseWriter, r *http.Request) (*SSE, bool) {
	s, ok := newSSE(w, r)
	if !ok {
		return nil, false
	}
	return &SSE{s: s}, true
}

// Comment writes an SSE comment, which browsers ignore - sent first, it
// opens the stream at once (EventSource reports it open) rather than at
// the first event.
func (s *SSE) Comment(text string) error { return s.s.write(": " + text + "\n\n") }

// Send writes one event ("" for the default "message" event). An error
// for the browser goes out as "failure", never "error" (EventSource's
// own connection-error event).
func (s *SSE) Send(event, payload string) error { return s.s.send(event, payload) }

// FactoryExtensions is the extensions the image factory builds, from
// its newest release that offers them (cached).
func FactoryExtensions(ctx context.Context) ([]schematic.CatalogEntry, error) {
	c, err := factoryCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return c.Extensions, nil
}

// ApplyNetwork puts cfg on trial on node and confirms it - from the
// address the node is reachable at afterwards, recorded in st if it
// moved. Unconfirmed, the node goes back to its previous configuration
// by itself, and this says why.
func ApplyNetwork(ctx context.Context, node *store.Node, st *store.Store, cfg *janusv1alpha1.NetworkConfig) error {
	res, _, err := applyNetwork(ctx, node, cfg, 60)
	if err != nil {
		return err
	}
	confirmNetwork(node, st, res)
	if !res.Confirmed {
		return errors.New(res.Error)
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	return nil
}

// Upgrade installs the update bundle at source on node (the node fetches
// it and checks its signature) and follows it until the node reboots
// into it - confirmed by the node itself once healthy (wait_for_health).
func Upgrade(ctx context.Context, node *store.Node, source *janusv1alpha1.ImageSource) error {
	conn, err := dialNode(node)
	if err != nil {
		return err
	}
	stream, err := janusv1alpha1.NewLifecycleServiceClient(conn).Upgrade(ctx, &janusv1alpha1.UpgradeRequest{Source: source, WaitForHealth: true})
	if err != nil {
		return errors.New(status.Convert(err).Message())
	}
	var last string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The node reboots once it switched slots: the stream may
			// break then, as expected.
			if last == "rebooting" || last == "switching-slot" {
				return nil
			}
			return errors.New(status.Convert(err).Message())
		}
		last = msg.GetStage()
	}
	if last == "" {
		return errors.New("the upgrade reported nothing")
	}
	return nil
}

// NodeVersion is the Janus version and image schematic node runs.
func NodeVersion(ctx context.Context, node *store.Node) (version, schematicID string, err error) {
	conn, err := dialNode(node)
	if err != nil {
		return "", "", err
	}
	v, err := janusv1alpha1.NewSystemServiceClient(conn).Version(ctx, &emptypb.Empty{})
	if err != nil {
		return "", "", errors.New(status.Convert(err).Message())
	}
	sc := v.GetSchematicId()
	if sc == "" {
		sc = schematic.DefaultID()
	}
	return v.GetVersion(), sc, nil
}

// Bundle is where a node fetches an update from.
type Bundle struct {
	Version   string
	Schematic string
	BaseURL   string // the directory rootfs.squashfs, rootfs.verity, uki-*.efi are in
	SHA256    string // rootfs.squashfs's
}

// ResolveBundle finds version's update bundle built with extensions -
// GitHub's release for the default schematic, the image factory's build
// otherwise (State "building" in the error: ask again later).
func ResolveBundle(ctx context.Context, version string, extensions []string) (*Bundle, string, error) {
	img, err := ResolveVMImage(ctx, version, extensions, "rootfs.squashfs")
	if err != nil {
		return nil, "", err
	}
	if img.State != "ready" {
		msg := img.Message
		if msg == "" {
			msg = "the update bundle is " + img.State
		}
		return nil, img.State, errors.New(msg)
	}
	base := strings.TrimSuffix(img.URL, "/rootfs.squashfs")
	if base == img.URL {
		return nil, "", fmt.Errorf("unexpected bundle URL %s", img.URL)
	}
	return &Bundle{Version: img.Version, Schematic: img.Schematic, BaseURL: base, SHA256: img.SHA256}, "ready", nil
}
