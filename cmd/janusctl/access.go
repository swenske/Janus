package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

var accessUsage = []string{
	"access trust                       the fleet the node trusts besides its own CA: root, bundle, issuing CAs",
	"access trust-set [-root FILE] BUNDLE  pin the fleet's root (the first time) and apply BUNDLE, signed by it and newer than the node's",
	"access trust-reset                 forget the fleet - only with a certificate of the node's own CA",
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
