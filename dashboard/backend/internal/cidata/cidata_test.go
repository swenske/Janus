package cidata

import (
	"os"
	"path/filepath"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/nocloud"
)

// The node's own reader must find and parse what Build writes - the
// only test that matters for this package.
func TestBuildReadsBackWithNoCloud(t *testing.T) {
	ud := UserData{
		ControllerAddress: "10.200.10.10:8443",
		ControllerCACert:  "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n",
		RegistrationToken: "0123456789abcdef",
		Network: &janusv1alpha1.NetworkConfig{
			Hostname: "lab-janus-03",
			Interfaces: []*janusv1alpha1.NetworkInterface{{
				Name: "mgmt", Mac: "52:54:00:c8:0a:17",
				Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"10.200.10.23/24"},
			}},
		},
	}
	img, err := Build(t.TempDir(), ud)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cidata.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := nocloud.FindVolume([]string{path})
	if err != nil {
		t.Fatalf("FindVolume: %v", err)
	}
	cfg, err := nocloud.Read(found)
	if err != nil {
		t.Fatalf("nocloud.Read: %v", err)
	}
	if cfg.ControllerAddress != ud.ControllerAddress || string(cfg.ControllerCACert) != ud.ControllerCACert || cfg.RegistrationToken != ud.RegistrationToken {
		t.Errorf("read back %+v", cfg)
	}
	if cfg.Network.GetHostname() != "lab-janus-03" || cfg.Network.GetInterfaces()[0].GetMac() != "52:54:00:c8:0a:17" {
		t.Errorf("network read back as %v", cfg.Network)
	}
}

func TestBuildRefusesAnInvalidNetwork(t *testing.T) {
	_, err := Build(t.TempDir(), UserData{ControllerAddress: "c:8443", ControllerCACert: "pem", Network: &janusv1alpha1.NetworkConfig{Hostname: "not a hostname"}})
	if err == nil {
		t.Fatal("Build accepted an invalid network")
	}
}
