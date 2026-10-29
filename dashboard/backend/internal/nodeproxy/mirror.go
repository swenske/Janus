// The image factory's mirror: when the factory can't be reached - no
// connection, no answer in time, a 5xx from its front end - nodes with
// extensions still get their update, from a read-only copy of the update
// bundles the factory built for its two newest releases (the site in
// mirror mode: janus-mirror.sw-servers.net for janus.sw-servers.net,
// docs/image-factory.md). Its bundles are the factory's own: the node
// checks their SHA-256 and the UKIs' signature wherever they come from.
package nodeproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultImageFactory is ImageFactoryURL's default, and
// DefaultImageFactoryMirror its mirror.
const (
	DefaultImageFactory       = "https://janus.sw-servers.net"
	DefaultImageFactoryMirror = "https://janus-mirror.sw-servers.net"
)

// ImageFactoryMirrorURL is asked when the image factory can't be reached;
// empty: none. Set once at startup (dashboardd's -image-factory-mirror,
// MirrorFor).
var ImageFactoryMirrorURL = ""

// MirrorFor is the mirror dashboardd's -image-factory-mirror names for
// the image factory factory: "auto" is the default factory's own mirror
// when factory is the default one, none with another; anything else is
// taken as given ("" for none).
func MirrorFor(factory, mirror string) string {
	if mirror != "auto" {
		return strings.TrimRight(mirror, "/")
	}
	if strings.TrimRight(factory, "/") == DefaultImageFactory {
		return DefaultImageFactoryMirror
	}
	return ""
}

// factoryTryTimeout bounds asking the factory when its mirror can answer
// instead: a network that's down otherwise holds the answer until the
// whole fetch's deadline, and the mirror never gets asked.
var factoryTryTimeout = 10 * time.Second

// unreachableError is an image factory failure that isn't its answer: no
// connection, a timeout, a 5xx.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

// withMirror asks the image factory at base - ask(ctx, base, false) - and,
// when it can't be reached, its mirror: ask(ctx, mirror, true). via is
// "mirror" when the mirror answered.
func withMirror(ctx context.Context, base string, ask func(ctx context.Context, base string, mirror bool) error) (via string, err error) {
	mirror := strings.TrimRight(ImageFactoryMirrorURL, "/")
	if mirror == "" || mirror == base {
		return "", ask(ctx, base, false)
	}
	fctx, cancel := context.WithTimeout(ctx, factoryTryTimeout)
	err = ask(fctx, base, false)
	cancel()
	var down *unreachableError
	if err == nil || !errors.As(err, &down) {
		return "", err
	}
	if merr := ask(ctx, mirror, true); merr != nil {
		return "", fmt.Errorf("%w - and %w", err, merr)
	}
	return "mirror", nil
}
