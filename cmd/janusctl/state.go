package main

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// runState: a node's state carried to its replacement
// (docs/guide/replacing-a-node.md) - exported to a file or a pipe,
// imported from one.
func runState(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewSystemServiceClient(conn)
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch args[0] {
	case "export":
		fs := flag.NewFlagSet("system state export", flag.ExitOnError)
		identity := fs.Bool("identity", false, "the node's CA and keys too: the replacement is this node to its clients")
		kit := fs.String("kit", "", "with -identity: the fleet's recovery kit - the identity goes only to a certificate the fleet's root itself signs, or the node's own CA")
		parseAnywhere(fs, args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system state export [-identity [-kit KIT]] FILE|-")
			os.Exit(2)
		}
		if *kit != "" {
			kc, err := kitDial(stateFrom.endpoint, stateFrom.ctxName, stateFrom.ctx, stateFrom.node, stateFrom.ca, *kit)
			if err != nil {
				log.Fatalf("janusctl: %v", err)
			}
			defer kc.Close()
			client = janusv1alpha1.NewSystemServiceClient(kc)
		}
		out := fs.Arg(0)
		w := io.Writer(os.Stdout)
		var f *os.File
		if out == "-" {
			if isTerminal(os.Stdout) {
				log.Fatal("the state is a tar holding private keys: not to a terminal - give a file, or pipe it into janusctl system state import -")
			}
		} else {
			var err error
			// Never over another file: an export holds keys.
			if f, err = os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
				log.Fatal(err)
			}
			w = f
		}
		stream, err := client.StateExport(c, &janusv1alpha1.StateExportRequest{IncludeIdentity: *identity})
		check("StateExport", err)
		n := 0
		for {
			d, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				if f != nil {
					f.Close()
					os.Remove(out)
				}
				check("StateExport", err)
			}
			if _, err := w.Write(d.GetBytes()); err != nil {
				log.Fatal(err)
			}
			n += len(d.GetBytes())
		}
		if f != nil {
			if err := f.Sync(); err != nil {
				log.Fatal(err)
			}
			if err := f.Close(); err != nil {
				log.Fatal(err)
			}
			fmt.Fprintf(os.Stderr, "State written to %s (%d bytes). It holds private keys: keep it like one, and delete it once imported.\n", out, n)
		}
	case "import":
		fs := flag.NewFlagSet("system state import", flag.ExitOnError)
		identity := fs.Bool("identity", false, "take the exported node's identity (the export must carry it): its credentials work here after the reboot, this node's own stop working")
		network := fs.Bool("network", false, "put the exported network configuration in place too: the node takes the other's addresses at its reboot")
		saveNetwork := fs.String("save-network", "", "without -network: where to save the exported network configuration, for the takeover (janusctl network apply FILE)")
		noReboot := fs.Bool("no-reboot", false, "don't reboot: the import waits for janusd's next start")
		from := fs.String("from", "", "pull the state from this node itself, node to node - a node of the context, or HOST:PORT with -from-ca/-cert/-key")
		fromAddress := fs.String("from-address", "", "with -from: where this node reaches the other (default: where janusctl does)")
		fromCA := fs.String("from-ca", "", "with -from HOST:PORT: the other node's CA (default: -ca)")
		fromCert := fs.String("from-cert", "", "with -from HOST:PORT: an admin certificate of the other node (default: -cert)")
		fromKey := fs.String("from-key", "", "with -from HOST:PORT: its key (default: -key)")
		kit := fs.String("kit", "", "with -from and -identity: the fleet's recovery kit - the other node grants its identity only to a certificate the fleet's root itself signs, or its own CA")
		parseAnywhere(fs, args[1:])
		if (*from == "") != (fs.NArg() == 1) || fs.NArg() > 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system state import [-identity] [-network | -save-network FILE] [-no-reboot] FILE|- | -from NODE [-kit KIT]")
			os.Exit(2)
		}
		opts := &janusv1alpha1.StateImportOptions{ReuseIdentity: *identity, ApplyNetwork: *network, NoReboot: *noReboot}
		if *from != "" {
			resp := importFrom(c, client, opts, *from, *fromAddress, *fromCA, *fromCert, *fromKey, *kit)
			printImported(resp, *network, *saveNetwork)
			return
		}
		r := io.Reader(os.Stdin)
		if in := fs.Arg(0); in != "-" {
			f, err := os.Open(in)
			if err != nil {
				log.Fatal(err)
			}
			defer f.Close()
			r = f
		}
		stream, err := client.StateImport(c)
		check("StateImport", err)
		// io.EOF from Send: the node stopped reading - CloseAndRecv says why.
		if err := stream.Send(&janusv1alpha1.StateImportChunk{Options: opts}); err == nil {
			buf := make([]byte, 64<<10)
			for {
				n, rerr := r.Read(buf)
				if n > 0 {
					if err := stream.Send(&janusv1alpha1.StateImportChunk{Data: append([]byte(nil), buf[:n]...)}); err != nil {
						break
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					log.Fatal(rerr)
				}
			}
		}
		resp, err := stream.CloseAndRecv()
		check("StateImport", err)
		printImported(resp, *network, *saveNetwork)
	default:
		fmt.Fprintf(os.Stderr, "janusctl system state: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

// printImported says what an import staged - saving the held network
// configuration to saveNetwork when asked.
func printImported(resp *janusv1alpha1.StateImportResponse, network bool, saveNetwork string) {
	fmt.Printf("Imported:   %s's state (%s, %d files)\n", resp.GetSourceHostname(), resp.GetSourceVersion(), len(resp.GetFiles()))
	if resp.GetIdentityReused() {
		fmt.Printf("Identity:   %s's - its credentials work here once the node restarts; this node's own stop working\n", resp.GetSourceHostname())
	} else {
		fmt.Println("Identity:   this node's own")
	}
	switch held := resp.GetHeldNetworkConfig(); {
	case network:
		fmt.Println("Network:    the exported configuration, at the restart")
	case len(held) > 0 && saveNetwork != "":
		if err := os.WriteFile(saveNetwork, held, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Network:    this node's own; the exported one saved to %s - janusctl network apply %s takes the other's addresses over\n", saveNetwork, saveNetwork)
	case len(held) > 0:
		fmt.Println("Network:    this node's own; the exported one held back (-save-network FILE keeps it)")
	default:
		fmt.Println("Network:    this node's own (the exported node had no saved configuration)")
	}
	for _, s := range resp.GetSkipped() {
		fmt.Println("Left out:  ", s)
	}
	for _, w := range resp.GetWarnings() {
		fmt.Println("Warning:   ", w)
	}
	if resp.GetRebooting() {
		fmt.Println("Rebooting:  the state goes in place as the node starts again")
	} else {
		fmt.Println("Staged:     the state goes in place at janusd's next start")
	}
}

// stateFrom is what -from resolves against: the context main used, or
// the credentials given on the command line (main sets it).
var stateFrom struct {
	ctxName       string
	ctx           *cliContext
	node          *ctxNode // the context's node janusctl reaches
	endpoint      string   // where janusctl reaches it
	ca, cert, key string
}

// importFrom has the node behind client pull the state of node from
// itself (docs/guide/replacing-a-node.md "Node to node"): the node makes
// a key and a signing request, the other node signs it for one export
// (janusctl reaching it with its own credentials), the node pulls. Only
// a request and a certificate pass through here - no key, no state.
func importFrom(c context.Context, client janusv1alpha1.SystemServiceClient, opts *janusv1alpha1.StateImportOptions, from, address, caFile, certFile, keyFile, kit string) *janusv1alpha1.StateImportResponse {
	var (
		conn     *grpc.ClientConn
		endpoint string
		err      error
	)
	if ctx := stateFrom.ctx; ctx != nil && caFile == "" && certFile == "" && keyFile == "" {
		nodes, err := selectNodes(ctx, from, false)
		if err != nil {
			log.Fatalf("janusctl: -from: %v", err)
		}
		endpoint = nodes[0].Address
		if kit != "" {
			conn, err = kitDial(endpoint, stateFrom.ctxName, ctx, &nodes[0], "", kit)
		} else {
			var tlsConfig *tls.Config
			if tlsConfig, err = nodeTLS(stateFrom.ctxName, ctx, nodes[0]); err != nil {
				log.Fatalf("janusctl: -from: %v", err)
			}
			conn, err = dialTLS(endpoint, tlsConfig)
		}
		if err != nil {
			log.Fatalf("janusctl: -from: %v", err)
		}
	} else {
		endpoint = from
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			endpoint = net.JoinHostPort(endpoint, "9505")
		}
		if kit != "" {
			conn, err = kitDial(endpoint, "", nil, nil, cmp.Or(caFile, stateFrom.ca), kit)
		} else {
			conn, err = dial(endpoint, cmp.Or(caFile, stateFrom.ca), cmp.Or(certFile, stateFrom.cert), cmp.Or(keyFile, stateFrom.key))
		}
		if err != nil {
			log.Fatal(err)
		}
	}
	defer conn.Close()
	csr, err := client.CertificateRequest(c, &janusv1alpha1.CertificateRequestRequest{Purpose: janusv1alpha1.CertificatePurpose_CERTIFICATE_PURPOSE_STATE_TRANSFER})
	check("CertificateRequest", err)
	grant, err := janusv1alpha1.NewSystemServiceClient(conn).StateExportGrant(c, &janusv1alpha1.StateExportGrantRequest{Csr: csr.GetCsr(), IncludeIdentity: opts.GetReuseIdentity()})
	check("StateExportGrant ("+from+")", err)
	resp, err := client.StateImportFrom(c, &janusv1alpha1.StateImportFromRequest{
		Address: cmp.Or(address, endpoint), Ca: grant.GetCa(), Certificate: grant.GetCertificate(), Options: opts,
	})
	check("StateImportFrom", err)
	return resp
}

// kitDial reaches the node at endpoint with a certificate the fleet's
// root itself signs - os:admin, an hour, for a key made here and kept in
// this process -, the recovery kit at kitPath opened here: what a node
// asks before its identity leaves it (StateExport, StateExportGrant with
// the identity; security review 2026-11-05 #5). The node is checked as
// the call would: by the context's CA for it (node), else the CA file
// caFile.
func kitDial(endpoint, ctxName string, ctx *cliContext, node *ctxNode, caFile, kitPath string) (*grpc.ClientConn, error) {
	var caPEM []byte
	user := ""
	if node != nil {
		caPEM, user = []byte(node.CAPEM), ctx.User
	} else {
		var err error
		if caPEM, err = os.ReadFile(caFile); err != nil {
			return nil, fmt.Errorf("-kit: the node's CA: %w", err)
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("-kit: no CA certificate to check the node against")
	}
	_, root, err := openKitFile(kitPath)
	if err != nil {
		return nil, fmt.Errorf("-kit: %w", err)
	}
	if ctx != nil {
		if err := sameFleet(filepath.Join(contextDir(ctxName), "cert.pem"), root.Cert); err != nil {
			return nil, fmt.Errorf("-kit: %w", err)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafPEM, err := root.IssueFor(key.Public(), pki.IssueOptions{CommonName: kitUser(user), Roles: []string{pki.RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: kitCertValidity})
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(leafPEM)
	return dialTLS(endpoint, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{b.Bytes}, PrivateKey: key}}, RootCAs: pool, MinVersion: tls.VersionTLS13})
}
