package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The Controller's own identity (internal/identity) over time: its
// certificate renewed 35 days before it ends, the nodes that reach Let's
// Encrypt through the relay given its CA to check the relay with.

// identityEvery is how often the identity is looked at; a var, the tests
// change it.
var identityEvery = 12 * time.Hour

func (a *app) identityLoop(ctx context.Context) {
	// The nodes first connect: the relay's settings are checked a little
	// after the start.
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	tick := time.NewTicker(identityEvery)
	defer tick.Stop()
	var said time.Time
	for {
		a.refreshIdentity(&said)
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			a.syncRelayTrust(ctx)
		case <-tick.C:
			a.syncRelayTrust(ctx)
		}
	}
}

// refreshIdentity renews the identity's certificate when it's due, and
// says once a day - said: when it last did - what a Controller still
// serving its first certificate is about to change.
func (a *app) refreshIdentity(said *time.Time) {
	if _, err := a.identity.Refresh(); err != nil {
		log.Printf("identity: renew the Controller's certificate: %v", err)
	}
	if at := a.identity.SwitchAt(); !at.IsZero() && time.Since(*said) >= 24*time.Hour {
		*said = time.Now()
		log.Printf("identity: until %s the Controller serves its first certificate, then one its identity CA signs: nodes given its certificate alone before (a seeded image, a NoCloud user-data) can't register after that, nor can Terraform, Prometheus or scripts reach it with that certificate - give them controller-info's ca_cert_pem instead", at.UTC().Format(time.DateOnly))
	}
}

// syncRelayTrust gives the identity's CA to the nodes reaching Let's
// Encrypt through the Controller's relay that check it with something
// else - the Controller's certificate alone, as it gave them before it
// kept its CA: that certificate is renewed.
func (a *app) syncRelayTrust(ctx context.Context) {
	for _, n := range a.store.List() {
		if ctx.Err() != nil {
			return
		}
		if err := a.syncNodeRelayTrust(ctx, n); err != nil {
			log.Printf("identity: node %s (%s): give its Let's Encrypt relay settings the identity CA: %v", n.Name, n.ID, err)
		}
	}
}

func (a *app) syncNodeRelayTrust(ctx context.Context, n *store.Node) error {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return err
	}
	c := janusv1alpha1.NewHAProxyServiceClient(conn)
	call := func() (context.Context, context.CancelFunc) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		return nodeproxy.WithUser(cctx, nodeproxy.ACMEOrderingUser), cancel
	}
	cctx, cancel := call()
	st, err := c.ACMEStatus(cctx, &emptypb.Empty{})
	cancel()
	if err != nil {
		return nil // unreachable now, or too old for Let's Encrypt: the next pass
	}
	if st.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED || st.GetPathMode() != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY {
		return nil
	}
	cctx, cancel = call()
	got, err := c.ACMEGetConfig(cctx, &emptypb.Empty{})
	cancel()
	if err != nil {
		return status.Convert(err).Err()
	}
	cfg := got.GetConfig()
	if cfg.GetPath().GetMode() != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY || a.identity.HoldsCA([]byte(cfg.GetPath().GetProxyCa())) {
		return nil
	}
	// The secrets come back empty: applied empty, the node keeps its own.
	cfg.Path.ProxyCa = string(a.identity.TrustPEM())
	cctx, cancel = call()
	resp, err := c.ACMEApplyConfig(cctx, &janusv1alpha1.ACMEApplyConfigRequest{Config: cfg})
	cancel()
	if err != nil {
		return status.Convert(err).Err()
	}
	if !resp.GetAccepted() {
		return fmt.Errorf("refused: %s", strings.Join(resp.GetErrors(), "; "))
	}
	log.Printf("identity: node %s (%s): its Let's Encrypt relay settings now check the Controller with its identity CA", n.Name, n.ID)
	return nil
}
