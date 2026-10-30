package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/logship"
	"github.com/swenske/Janus/internal/tlstrust"
)

func TestSetPaused(t *testing.T) {
	cfg := func() *janusv1alpha1.LogForwardingConfig {
		return &janusv1alpha1.LogForwardingConfig{Targets: []*janusv1alpha1.LogTarget{{Name: "nas"}, {Name: "elastic", Disabled: true}}}
	}
	c := cfg()
	if changed, err := setPaused(c, "nas", true); err != nil || !changed || !c.GetTargets()[0].GetDisabled() || !c.GetTargets()[1].GetDisabled() {
		t.Fatalf("pause nas: %v %v %v", changed, err, c)
	}
	if changed, err := setPaused(c, "nas", true); err != nil || changed {
		t.Errorf("pause nas again: %v %v", changed, err)
	}
	c = cfg()
	if changed, err := setPaused(c, "elastic", false); err != nil || !changed || c.GetTargets()[1].GetDisabled() || c.GetTargets()[0].GetDisabled() {
		t.Fatalf("resume elastic: %v %v %v", changed, err, c)
	}
	if _, err := setPaused(cfg(), "syslog", true); err == nil || !strings.Contains(err.Error(), "nas, elastic") {
		t.Errorf("an unknown target: %v", err)
	}
	if _, err := setPaused(&janusv1alpha1.LogForwardingConfig{}, "nas", true); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("no target at all: %v", err)
	}
	managed := cfg()
	managed.ManagedBy = "janus-controller 0f1e2d3c profile web 1a2b3c4d"
	changed, err := setPaused(managed, "nas", true)
	if err == nil || changed || managed.GetTargets()[0].GetDisabled() || !strings.Contains(err.Error(), "(janus-controller 0f1e2d3c profile web)") {
		t.Errorf("a profile's configuration: %v %v", changed, err)
	}
}

// janusctl system log-forwarding pause|resume against the node's own
// LogForwardingSet: the configuration goes back without its secrets (as
// LogForwardingGet gives it), the node keeps them; the target paused,
// then sending again.
func TestPauseTargetKeepsTheSecrets(t *testing.T) {
	dir := t.TempDir()
	ship := logship.New(logship.Options{})
	defer ship.Close()
	sys := &api.System{LogShip: ship, LogShipDir: dir}
	gs := grpc.NewServer()
	janusv1alpha1.RegisterSystemServiceServer(gs, sys)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(ln) }()
	defer gs.Stop()
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := janusv1alpha1.NewSystemServiceClient(conn)
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A client certificate and its key (a secret).
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node"}, NotAfter: time.Now().Add(time.Hour)},
		&x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))

	// Nothing listens there: the targets never send, which is fine.
	full := logship.Config{Targets: []logship.Target{
		{Name: "elastic", Elasticsearch: &logship.ElasticsearchTarget{URL: "https://127.0.0.1:1", APIKey: "the-api-key"}},
		{Name: "nas", Syslog: &logship.SyslogTarget{Address: "127.0.0.1:1", Transport: "tls"}, TLS: tlstrust.Trust{ClientCertPEM: certPEM, ClientKeyPEM: keyPEM}},
		{Name: "es-user", Elasticsearch: &logship.ElasticsearchTarget{URL: "http://127.0.0.1:1", Username: "janus", Password: "the-password"}},
	}}
	if _, err := client.LogForwardingSet(c, logship.ToProto(full)); err != nil {
		t.Fatal(err)
	}
	stateOf := func(resp *janusv1alpha1.LogForwardingResponse, name string) string {
		for _, st := range resp.GetTargets() {
			if st.GetName() == name {
				return st.GetState()
			}
		}
		return "?"
	}
	for _, name := range []string{"elastic", "nas", "es-user"} {
		resp, changed, err := pauseTarget(c, client, name, true)
		if err != nil || !changed || stateOf(resp, name) != "disabled" {
			t.Fatalf("pause %s: %v %v %s", name, changed, err, stateOf(resp, name))
		}
	}
	saved, err := logship.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range saved.Targets {
		if !tg.Disabled {
			t.Errorf("%s isn't paused: %+v", tg.Name, tg)
		}
	}
	if saved.Targets[0].Elasticsearch.APIKey != "the-api-key" || saved.Targets[1].TLS.ClientKeyPEM != keyPEM || saved.Targets[2].Elasticsearch.Password != "the-password" {
		t.Fatalf("a secret was lost on the way: %+v", saved)
	}
	if _, changed, err := pauseTarget(c, client, "nas", true); err != nil || changed {
		t.Errorf("pause nas again: %v %v", changed, err)
	}
	resp, changed, err := pauseTarget(c, client, "nas", false)
	if err != nil || !changed || stateOf(resp, "nas") == "disabled" || stateOf(resp, "elastic") != "disabled" {
		t.Fatalf("resume nas: %v %v nas %s, elastic %s", changed, err, stateOf(resp, "nas"), stateOf(resp, "elastic"))
	}
	if saved, _ = logship.LoadConfig(dir); saved.Targets[1].Disabled || saved.Targets[1].TLS.ClientKeyPEM != keyPEM {
		t.Fatalf("resumed: %+v", saved.Targets[1])
	}
	if _, _, err := pauseTarget(c, client, "nope", true); err == nil {
		t.Error("an unknown target was paused")
	}
}
