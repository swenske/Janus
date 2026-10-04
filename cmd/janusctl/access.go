package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

var accessUsage = []string{
	"access trust                       the fleet the node trusts besides its own CA: root, bundle, issuing CAs",
	"access trust-set [-root FILE] BUNDLE  pin the fleet's root (the first time) and apply BUNDLE, signed by it and newer than the node's",
	"access trust-reset                 forget the fleet - only with a certificate of the node's own CA",
	"access rotate-ca [-console] DIR    replace the node's own CA: every certificate it issued stops working; writes DIR/{ca.crt,admin.crt,admin.key}, the key made here and never sent (-console: the node makes it and prints it on its console; only DIR/ca.crt)",
}

func runAccess(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewAccessServiceClient(conn)
	c, cancel := ctx()
	defer cancel()
	var st *janusv1alpha1.TrustState
	var err error
	switch args[0] {
	case "trust":
		st, err = client.TrustGet(c, &emptypb.Empty{})
		check("TrustGet", err)
	case "trust-set":
		fs := flag.NewFlagSet("access trust-set", flag.ExitOnError)
		root := fs.String("root", "", "the fleet's root certificate (PEM) - needed the first time only")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl access trust-set [-root FILE] BUNDLE")
			os.Exit(2)
		}
		req := &janusv1alpha1.TrustSetRequest{}
		if *root != "" {
			req.RootCert = readOrDie(*root)
		}
		req.Bundle = readOrDie(fs.Arg(0))
		st, err = client.TrustSet(c, req)
		check("TrustSet", err)
	case "trust-reset":
		st, err = client.TrustReset(c, &emptypb.Empty{})
		check("TrustReset", err)
	case "rotate-ca":
		rotateCA(c, client, args[1:])
		return
	default:
		fmt.Fprintf(os.Stderr, "janusctl access: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
	printTrust(st)
}

func readOrDie(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return data
}

func printTrust(st *janusv1alpha1.TrustState) {
	if len(st.GetRootCert()) == 0 {
		fmt.Println("Fleet:   none - only the node's own CA lets in")
		return
	}
	describe := func(p []byte) string {
		block, _ := pem.Decode(p)
		if block == nil {
			return "(not a certificate)"
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "(" + err.Error() + ")"
		}
		sum := sha256.Sum256(c.Raw)
		return fmt.Sprintf("%s (SHA-256 %s, until %s)", c.Subject.CommonName, hex.EncodeToString(sum[:]), c.NotAfter.UTC().Format(time.DateOnly))
	}
	fmt.Println("Root:   ", describe(st.GetRootCert()))
	if st.GetBundleVersion() == 0 {
		fmt.Println("Bundle:  none - only certificates the root signed itself let in")
		return
	}
	fmt.Printf("Bundle:  version %d, issued %s\n", st.GetBundleVersion(), time.Unix(st.GetBundleIssuedUnix(), 0).UTC().Format(time.RFC3339))
	for _, ca := range st.GetIssuingCas() {
		fmt.Println("Issuing:", describe(ca))
	}
}

// rotateCA replaces the node's own CA; the new admin certificate's key is
// made here and never leaves this machine - unless -console.
func rotateCA(c context.Context, client janusv1alpha1.AccessServiceClient, args []string) {
	fs := flag.NewFlagSet("access rotate-ca", flag.ExitOnError)
	console := fs.Bool("console", false, "have the node make the admin key and print it on its console, like at first boot")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: janusctl access rotate-ca [-console] DIR")
		os.Exit(2)
	}
	dir := fs.Arg(0)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	req := &janusv1alpha1.LocalCARotateRequest{}
	var keyPEM []byte
	if !*console {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			log.Fatal(err)
		}
		pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			log.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			log.Fatal(err)
		}
		req.AdminPublicKey = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	resp, err := client.LocalCARotate(c, req)
	check("LocalCARotate", err)
	files := map[string][]byte{"ca.crt": resp.GetCaCert()}
	if !*console {
		files["admin.crt"], files["admin.key"] = resp.GetAdminCert(), keyPEM
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			log.Fatalf("write %s: %v", name, err)
		}
	}
	if *console {
		fmt.Printf("The node's own CA is replaced: its new CA certificate is in %s/ca.crt; the new admin certificate and key are on its console.\n", dir)
		return
	}
	fmt.Printf("The node's own CA is replaced: wrote %s/{ca.crt,admin.crt,admin.key}. Every certificate the old CA issued no longer works.\n", dir)
}
