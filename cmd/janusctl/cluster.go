package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// runCluster: the node's membership of a cluster (docs/cluster.md) -
// the primitives an orchestrator drives, by hand here.
func runCluster(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewClusterServiceClient(conn)
	c, cancel := ctx()
	defer cancel()
	switch args[0] {
	case "get":
		info, err := client.ClusterGet(c, &emptypb.Empty{})
		check("ClusterGet", err)
		printCluster(info)
	case "status":
		st, err := client.ClusterStatus(c, &emptypb.Empty{})
		check("ClusterStatus", err)
		printClusterStatus(st)
	case "csr":
		fs := flag.NewFlagSet("cluster csr", flag.ExitOnError)
		_ = fs.Parse(args[1:])
		if fs.NArg() > 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl cluster csr [OUT]")
			os.Exit(2)
		}
		resp, err := janusv1alpha1.NewSystemServiceClient(conn).CertificateRequest(c, &janusv1alpha1.CertificateRequestRequest{Purpose: janusv1alpha1.CertificatePurpose_CERTIFICATE_PURPOSE_CLUSTER_MEMBER})
		check("CertificateRequest", err)
		if fs.NArg() == 0 {
			os.Stdout.Write(resp.GetCsr())
			return
		}
		if err := os.WriteFile(fs.Arg(0), resp.GetCsr(), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Signing request written to", fs.Arg(0), "- sign it as the member certificate and bring it back with cluster set -cert")
	case "set":
		fs := flag.NewFlagSet("cluster set", flag.ExitOnError)
		id := fs.String("id", "", "the cluster's id (a UUID)")
		name := fs.String("name", "", "the cluster's name")
		member := fs.String("member", "", "this node's member name (default: its hostname)")
		port := fs.Uint("port", 0, "the peers port (default 10057)")
		caFile := fs.String("ca", "", "the cluster's CA certificate (PEM) - the first time, or to replace it")
		certFile := fs.String("cert", "", "the member certificate (PEM) for the last csr - the first time, or to renew it")
		managedBy := fs.String("managed-by", "", "who sets the composition")
		managed := fs.String("managed", "", "what the cluster manages on the node, comma-separated: haproxy, files, vrrp, bgp, firewall - locked to the orchestrator's calls")
		parseAnywhere(fs, args[1:])
		if fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "usage: janusctl cluster set -id UUID -name NAME [-member NAME] [-port N] [-ca FILE] [-cert FILE] MEMBER...  (MEMBER: name=address[,api=host:port])")
			os.Exit(2)
		}
		req := &janusv1alpha1.ClusterSetRequest{Id: *id, Name: *name, MemberName: *member, PeersPort: uint32(*port), ManagedBy: *managedBy}
		for _, d := range strings.Split(*managed, ",") {
			if d = strings.TrimSpace(d); d != "" {
				req.Managed = append(req.Managed, d)
			}
		}
		for _, arg := range fs.Args() {
			mb, err := parseMember(arg)
			if err != nil {
				log.Fatalf("member %q: %v", arg, err)
			}
			req.Members = append(req.Members, mb)
		}
		if req.MemberName == "" {
			info, err := client.ClusterGet(c, &emptypb.Empty{})
			check("ClusterGet", err)
			req.MemberName = info.GetMemberName()
		}
		if *caFile != "" {
			req.CaCert = readOrDie(*caFile)
		}
		if *certFile != "" {
			req.MemberCert = readOrDie(*certFile)
		}
		info, err := client.ClusterSet(c, req)
		check("ClusterSet", err)
		printCluster(info)
	case "leave":
		info, err := client.ClusterLeave(c, &emptypb.Empty{})
		check("ClusterLeave", err)
		printCluster(info)
	case "release":
		info, err := client.ClusterRelease(c, &janusv1alpha1.ClusterReleaseRequest{Domains: args[1:]})
		check("ClusterRelease", err)
		printCluster(info)
	case "drain":
		fs := flag.NewFlagSet("cluster drain", flag.ExitOnError)
		timeout := fs.Uint("timeout", 60, "seconds to wait for VRRP and BGP to show the node out of service")
		_ = fs.Parse(args[1:])
		cancel()
		dc, dcancel := context.WithTimeout(context.Background(), time.Duration(*timeout+10)*time.Second)
		defer dcancel()
		stream, err := client.ClusterDrain(dc, &janusv1alpha1.ClusterDrainRequest{TimeoutSeconds: uint32(*timeout)})
		check("ClusterDrain", err)
		for {
			p, err := stream.Recv()
			if err == io.EOF {
				break
			}
			check("ClusterDrain", err)
			fmt.Printf("%-8s %s\n", p.GetStage(), p.GetMessage())
			if p.GetStage() == "timeout" {
				os.Exit(1)
			}
		}
	case "undrain":
		info, err := client.ClusterUndrain(c, &emptypb.Empty{})
		check("ClusterUndrain", err)
		printCluster(info)
	case "pull":
		fs := flag.NewFlagSet("cluster pull", flag.ExitOnError)
		from := fs.String("from", "", "the member to pull from")
		items := fs.String("items", "", "what to pull, comma-separated: runtime-certs, files, acme-certs, tls-ticket-keys (default: all)")
		_ = fs.Parse(args[1:])
		if *from == "" {
			fmt.Fprintln(os.Stderr, "usage: janusctl cluster pull -from MEMBER [-items LIST]")
			os.Exit(2)
		}
		req := &janusv1alpha1.ClusterPullRequest{From: *from}
		for _, it := range strings.Split(*items, ",") {
			if it = strings.TrimSpace(it); it == "" {
				continue
			}
			v, ok := janusv1alpha1.ClusterItem_value["CLUSTER_ITEM_"+strings.ToUpper(strings.ReplaceAll(it, "-", "_"))]
			if !ok {
				log.Fatalf("unknown item %q", it)
			}
			req.Items = append(req.Items, janusv1alpha1.ClusterItem(v))
		}
		cancel()
		pc, pcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer pcancel()
		stream, err := client.ClusterPull(pc, req)
		check("ClusterPull", err)
		for {
			p, err := stream.Recv()
			if err == io.EOF {
				break
			}
			check("ClusterPull", err)
			if p.GetDone() {
				fmt.Println(p.GetMessage())
				continue
			}
			item := strings.ToLower(strings.TrimPrefix(p.GetItem().String(), "CLUSTER_ITEM_"))
			fmt.Printf("%-16s %-30s %s\n", strings.ReplaceAll(item, "_", "-"), p.GetName(), p.GetMessage())
		}
	case "rotate-tickets":
		fs := flag.NewFlagSet("cluster rotate-tickets", flag.ExitOnError)
		renew := fs.Bool("renew", false, "replace every key of the ring at once - no key it held is used or accepted any more")
		_ = fs.Parse(args[1:])
		st, err := client.TLSTicketKeysRotate(c, &janusv1alpha1.TLSTicketKeysRotateRequest{Renew: *renew})
		check("TLSTicketKeysRotate", err)
		what, took := "rotated", "took the new key"
		if *renew {
			what, took = "renewed", "took the new ring"
		}
		fmt.Printf("TLS ticket key ring %s: %d keys, sha256 %s - HAProxy %s\n", what, st.GetKeys(), st.GetSha256(), map[bool]string{true: took, false: "doesn't use the ring yet"}[st.GetLoaded()])
	default:
		fmt.Fprintf(os.Stderr, "janusctl cluster: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

// parseMember reads "name=address[,api=host:port]".
func parseMember(s string) (*janusv1alpha1.ClusterMember, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return nil, fmt.Errorf("name=address expected")
	}
	address, api, _ := strings.Cut(rest, ",api=")
	if address == "" {
		return nil, fmt.Errorf("no address")
	}
	return &janusv1alpha1.ClusterMember{Name: name, Address: address, ApiAddress: api}, nil
}

func printCluster(info *janusv1alpha1.ClusterInfo) {
	if !info.GetMember() {
		fmt.Printf("Cluster:     none - the node is on its own, local peer %s\n", info.GetMemberName())
	} else {
		fmt.Printf("Cluster:     %s (%s)\n", info.GetName(), info.GetId())
		fmt.Printf("This node:   %s\n", info.GetMemberName())
		if t := info.GetMemberCertNotAfterUnix(); t > 0 {
			fmt.Printf("Member cert: valid until %s\n", time.Unix(t, 0).Local().Format(time.RFC3339))
		}
		if len(info.GetManaged()) > 0 {
			fmt.Printf("Managed:     %s (by %s)\n", strings.Join(info.GetManaged(), ", "), info.GetManagedBy())
			if info.GetAdvisory() {
				fmt.Println("Locks:       advisory - an admin's certificate with the cluster's id gets through (janusctl cluster)")
			} else {
				fmt.Println("Locks:       the Controller's certificate only")
			}
		}
	}
	if info.GetDrained() {
		fmt.Println("Drained:     yes - out of service (VRRP priority lowered, BGP routes withdrawn) until cluster undrain or a reboot")
	}
	fmt.Printf("Peers port:  %d\n", info.GetPeersPort())
	if tk := info.GetTlsTicketKeys(); tk != nil {
		fmt.Printf("Ticket keys: %d, changed %s, sha256 %s\n", tk.GetKeys(), time.Unix(tk.GetRotatedUnix(), 0).Local().Format(time.RFC3339), tk.GetSha256())
	}
	tw := table("MEMBER", "ADDRESS", "API")
	for _, m := range info.GetMembers() {
		api := m.GetApiAddress()
		if api == "" && m.GetAddress() != "" {
			api = m.GetAddress() + ":9505"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.GetName(), m.GetAddress(), api)
	}
	tw.Flush()
	if cfg := info.GetPeersConfig(); cfg != "" {
		fmt.Println("\nPeers section HAProxy runs with:")
		fmt.Print(cfg)
	}
}

func printClusterStatus(st *janusv1alpha1.ClusterStatusResponse) {
	printCluster(st.GetInfo())
	if len(st.GetPeers()) == 0 {
		return
	}
	fmt.Println()
	tw := table("PEER", "ADDRESS", "STATUS", "HANDSHAKE", "TABLES", "SYNCED")
	for _, p := range st.GetPeers() {
		status := p.GetLastStatus()
		switch {
		case p.GetLocal():
			status = "local"
		case p.GetUp():
			status = "up"
		}
		synced := 0
		for _, t := range p.GetTables() {
			if t.GetSynced() {
				synced++
			}
		}
		syncedText := fmt.Sprintf("%d/%d", synced, len(p.GetTables()))
		if p.GetLocal() {
			syncedText = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", p.GetName(), p.GetAddress(), status, p.GetLastHandshake(), len(p.GetTables()), syncedText)
	}
	tw.Flush()
}
