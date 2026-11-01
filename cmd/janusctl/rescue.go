package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/nocloud"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
)

// imageRescueVolume writes a rescue volume (nocloud.RescueLabel):
// attached to a node, it opens a rescue window at the next boot,
// whatever the node trusts (docs/guide/recovery.md).
func imageRescueVolume(args []string) {
	fs := flag.NewFlagSet("image rescue-volume", flag.ExitOnError)
	fat := fs.Bool("fat", false, "a FAT disk image (a USB stick, a disk on a hypervisor) - by default an ISO, to attach as a CD-ROM")
	skipFirewall := fs.Bool("skip-firewall", false, "the node doesn't apply its saved firewall ruleset that boot: it may shut the rescue window's port (9506)")
	parseAnywhere(fs, args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: janusctl image rescue-volume [-fat] [-skip-firewall] OUT")
		os.Exit(2)
	}
	out := fs.Arg(0)
	r := nocloud.Rescue{}
	if *skipFirewall {
		r.Firewall = "skip"
	}
	raw, err := json.Marshal(r)
	if err != nil {
		log.Fatal(err)
	}
	work, err := os.MkdirTemp(filepath.Dir(out), ".rescue-volume-")
	if err != nil {
		log.Fatalf("image rescue-volume: %v", err)
	}
	defer os.RemoveAll(work)
	if err := nocloud.WriteVolume(out, work, nocloud.RescueLabel, *fat, map[string][]byte{nocloud.RescueFileName: append(raw, '\n')}); err != nil {
		log.Fatalf("image rescue-volume: %v", err)
	}
	how := "attach it as a CD-ROM"
	if *fat {
		how = "attach it as a disk, or write it onto a USB stick"
	}
	fmt.Printf("Wrote the rescue volume %s: %s and restart the node - its console shows a rescue code for %s.\n", out, how, pairing.RescueLifetime)
	fmt.Println("Then: janusctl access rescue -endpoint ADDRESS -code CODE DIR. Detach the volume once done: each boot with it opens a window.")
}

// accessRescue asks a node's rescue window - started with a rescue
// volume - for an admin certificate of its own CA, an hour, for a key
// made here: written in DIR as ca.crt, admin.crt, admin.key, what -ca,
// -cert and -key take.
func accessRescue(args []string) {
	fs := flag.NewFlagSet("access rescue", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "the node's API, HOST[:9505] (required)")
	code := fs.String("code", "", "the rescue code its console shows (required)")
	pairingAddr := fs.String("pairing", "", "the node's rescue window, HOST:PORT (default: the endpoint's host, port "+pairing.DefaultPort+")")
	name := fs.String("name", defaultUser(), "who you are: the certificate is rescue:NAME, the node logs it")
	parseAnywhere(fs, args)
	if fs.NArg() != 1 || *endpoint == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "usage: janusctl access rescue -endpoint HOST[:9505] -code CODE [-pairing HOST:PORT] [-name NAME] DIR")
		os.Exit(2)
	}
	dir := fs.Arg(0)
	addr := *endpoint
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), "9505")
	}
	paddr := *pairingAddr
	if paddr == "" {
		host, _, _ := net.SplitHostPort(addr)
		paddr = net.JoinHostPort(host, pairing.DefaultPort)
	}
	files := map[string]string{"ca.crt": filepath.Join(dir, "ca.crt"), "admin.crt": filepath.Join(dir, "admin.crt"), "admin.key": filepath.Join(dir, "admin.key")}
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			log.Fatalf("janusctl access rescue: %s exists already", f)
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		log.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		log.Fatal(err)
	}
	c, cancel := ctx()
	defer cancel()
	resp, ca, err := pairing.Rescue(c, paddr, *code, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), *name)
	if err != nil {
		log.Fatalf("janusctl access rescue: %s: %v", paddr, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	for f, b := range map[string][]byte{
		files["ca.crt"]:    resp.GetCaCert(),
		files["admin.crt"]: resp.GetCert(),
		files["admin.key"]: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := writeNew(f, b); err != nil {
			log.Fatalf("janusctl access rescue: %v", err)
		}
	}
	until := time.Now().Add(pairing.RescueValidity).Format("15:04")
	flags := fmt.Sprintf("-endpoint %s -ca %s -cert %s -key %s", *endpoint, files["ca.crt"], files["admin.crt"], files["admin.key"])
	fmt.Printf("Rescued: an admin certificate of the node's own CA (SHA-256 %s) for rescue:%s, until %s, in %s.\n", pki.Fingerprint(ca.Raw), *name, until, dir)
	fmt.Printf("  janusctl %s access trust          what the node trusts\n", flags)
	fmt.Printf("  janusctl %s access trust-reset    it forgets its fleet\n", flags)
	fmt.Printf("  janusctl fleet adopt NAME %s      into your fleet\n", flags)
	fmt.Println("Detach the rescue volume: each boot with it opens a window.")
}

// writeNew writes a file that mustn't exist yet, 0600.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
