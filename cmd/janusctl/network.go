package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
)

// redialer opens a connection to another endpoint of the same node, with
// the same credentials - network apply confirms over the node's new
// address when the old one goes away.
type redialer func(endpoint string) (*grpc.ClientConn, error)

func runNetwork(conn *grpc.ClientConn, endpoint string, redial redialer, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewNetworkServiceClient(conn)
	switch args[0] {
	case "status":
		c, cancel := ctx()
		defer cancel()
		st, err := client.NetworkStatus(c, &emptypb.Empty{})
		check("NetworkStatus", err)
		printNetworkStatus(st)
	case "neighbors":
		runNeighbors(client, args[1:])
	case "get":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.NetworkConfigGet(c, &emptypb.Empty{})
		check("NetworkConfigGet", err)
		data, err := netconfig.Marshal(resp.GetConfig())
		check("encode", err)
		if resp.GetIsDefault() {
			fmt.Fprintln(os.Stderr, "# nothing configured - the defaults are in effect (kernel boot DHCP)")
		}
		os.Stdout.Write(data)
		fmt.Println()
	case "apply":
		runNetworkApply(client, endpoint, redial, args[1:])
	case "confirm":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.NetworkConfigConfirm(c, &emptypb.Empty{})
		check("NetworkConfigConfirm", err)
		fmt.Printf("confirmed via %s - the configuration is saved\n", resp.GetConfirmedVia())
	case "modules":
		runNetworkModules(client)
	case "firewall":
		runFirewall(conn, endpoint, redial, args[1:])
	case "vrrp":
		runVRRP(conn, args[1:])
	case "bgp":
		runBGP(conn, args[1:])
	case "consul":
		runConsul(conn, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "janusctl network: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func runNetworkApply(client janusv1alpha1.NetworkServiceClient, endpoint string, redial redialer, args []string) {
	fs := flag.NewFlagSet("network apply", flag.ExitOnError)
	timeout := fs.Duration("timeout", netconfig.DefaultConfirmTimeout, "how long the node waits for the confirmation before reverting (5s-5m)")
	noConfirm := fs.Bool("no-confirm", false, "apply only - confirm yourself with `janusctl network confirm`, over an address the new configuration keeps, before the timeout")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: janusctl network apply [-timeout 30s] [-no-confirm] FILE   (FILE: JSON as `janusctl network get` prints it; - for stdin)")
		os.Exit(2)
	}
	var data []byte
	var err error
	if fs.Arg(0) == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(fs.Arg(0))
	}
	check("read the configuration", err)
	cfg, err := netconfig.Parse(data)
	check("configuration", err)

	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	stream, err := client.NetworkConfigApply(c, &janusv1alpha1.NetworkConfigApplyRequest{Config: cfg, ConfirmTimeoutSeconds: uint32(timeout.Seconds())})
	check("NetworkConfigApply", err)
	var addrs []string
	var revertAt time.Time
	applying, lost, got := false, false, 0
	// A change taking the address this call came in on: its stream can't
	// outlive it, its first messages included (netconfig.ApplyGrace).
	var grace *time.Timer
	dropping := netconfig.DropsEndpoint(cfg, endpoint)
	if dropping {
		grace = time.AfterFunc(netconfig.ApplyGrace, cancel)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			code := status.Code(err)
			if (got == 0 && !dropping) || (code != codes.Unavailable && code != codes.DeadlineExceeded && code != codes.Canceled) {
				log.Fatalf("NetworkConfigApply: %v", err)
			}
			// Expected when the node dropped the address this call came
			// in on: the configuration is applied, just unreported - or,
			// "applying" lost with it too, maybe not: confirming tells.
			if applying {
				fmt.Println("(connection lost while applying - expected if the node's address changed)")
			} else {
				fmt.Println("(connection lost - checking whether the node applied it)")
			}
			lost = true
			break
		}
		got++
		fmt.Println(progressLine(os.Stdout, resp.GetStage(), 0, false, resp.GetMessage()))
		if resp.GetRevertAtUnix() != 0 {
			revertAt = time.Unix(resp.GetRevertAtUnix(), 0)
		}
		if resp.GetStage() == "applying" {
			applying = true
		}
		if len(resp.GetAddresses()) > 0 {
			addrs = resp.GetAddresses()
			fmt.Printf("addresses: %s\n", strings.Join(addrs, ", "))
		}
	}
	if grace != nil {
		grace.Stop()
	}
	if revertAt.IsZero() { // the node's own deadline lost with the connection
		revertAt = started.Add(*timeout)
	}
	if *noConfirm {
		fmt.Printf("not confirmed - run `janusctl network confirm` over an address the configuration keeps before %s, or it reverts\n", revertAt.Format(time.TimeOnly))
		return
	}

	candidates := netconfig.ConfirmEndpoints(endpoint, cfg, addrs, lost)
	deadline := revertAt.Add(-time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		for _, ep := range candidates {
			via, err := confirmOver(ep, redial)
			if err == nil {
				fmt.Printf("confirmed via %s (reached the node at %s) - the configuration is saved\n", via, ep)
				return
			}
			if status.Code(err) == codes.FailedPrecondition {
				log.Fatal(netconfig.NothingOnTrial(time.Now(), revertAt, status.Convert(err).Message()))
			}
			if attempt == 0 {
				fmt.Printf("confirm over %s: %s\n", ep, status.Convert(err).Message())
			}
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("couldn't confirm over %s before %s - the node reverts to its previous configuration by itself", strings.Join(candidates, ", "), revertAt.Format(time.TimeOnly))
}

func confirmOver(endpoint string, redial redialer) (string, error) {
	conn, err := redial(endpoint)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	c, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	resp, err := janusv1alpha1.NewNetworkServiceClient(conn).NetworkConfigConfirm(c, &emptypb.Empty{})
	if err != nil {
		return "", err // a status: FailedPrecondition is "nothing on trial"
	}
	return resp.GetConfirmedVia(), nil
}

func printNetworkStatus(st *janusv1alpha1.NetworkStatusResponse) {
	fmt.Printf("hostname: %s\n", st.GetHostname())
	if !st.GetManaged() {
		fmt.Println("(not managed by this janusd - observed only)")
	}
	if st.GetTrialPending() {
		fmt.Printf("TRIAL: a configuration awaits confirmation, reverting at %s\n", time.Unix(st.GetTrialRevertAtUnix(), 0).Format(time.TimeOnly))
	}
	fmt.Println("\ninterfaces:")
	for _, i := range st.GetInterfaces() {
		state := "down"
		if i.GetUp() {
			state = "up"
			if !i.GetCarrier() {
				state = "up, no carrier"
			}
		}
		kind := i.GetKind()
		if i.GetVlanParent() != "" {
			kind = fmt.Sprintf("vlan %d on %s", i.GetVlanId(), i.GetVlanParent())
		}
		mode := ""
		if st.GetManaged() && (i.GetKind() == "physical" || i.GetKind() == "vlan") {
			mode = " " + strings.ToLower(strings.TrimPrefix(i.GetMode().String(), "ADDRESSING_MODE_"))
		}
		fmt.Printf("  %-12s %-18s %s mtu %d (%s)%s\n", i.GetName(), kind, i.GetMac(), i.GetMtu(), state, mode)
		for _, a := range i.GetAddresses() {
			fmt.Printf("  %-12s   %s\n", "", a)
		}
		if l := i.GetDhcp(); l != nil {
			// No expiry: an older node's lease, the kernel's at boot.
			how := "boot, not renewed"
			if e := l.GetExpiresUnix(); e > 0 {
				how = "until " + time.Unix(e, 0).Format("2006-01-02 15:04:05")
			}
			fmt.Printf("  %-12s   dhcp (%s): %s from %s, router %s, dns %s, ntp %s\n", "", how, l.GetAddress(), l.GetServer(), l.GetRouter(), strings.Join(l.GetDnsServers(), " "), strings.Join(l.GetNtpServers(), " "))
		}
	}
	fmt.Println("\nroutes:")
	for _, r := range st.GetRoutes() {
		via := ""
		if r.GetGateway() != "" {
			via = " via " + r.GetGateway()
		}
		fmt.Printf("  %s%s dev %s metric %d\n", r.GetDestination(), via, r.GetInterface(), r.GetMetric())
	}
	fmt.Printf("\ndns: %s", strings.Join(st.GetDnsServers(), " "))
	if len(st.GetDnsSearch()) > 0 {
		fmt.Printf(" (search %s)", strings.Join(st.GetDnsSearch(), " "))
	}
	fmt.Println()
	if t := st.GetTime(); t != nil {
		sync := "not synchronized"
		if t.GetSynchronized() {
			sync = "synchronized"
		}
		fmt.Printf("time: %s, servers %s (%s)", sync, strings.Join(t.GetServers(), " "), t.GetSource())
		if t.GetLastSyncUnix() != 0 {
			fmt.Printf(", last sync %s with %s, offset %s, stratum %d", time.Unix(t.GetLastSyncUnix(), 0).Format(time.TimeOnly), t.GetLastServer(), time.Duration(t.GetOffsetNs()), t.GetStratum())
		}
		if t.GetError() != "" {
			fmt.Printf("\n      last error: %s", t.GetError())
		}
		fmt.Println()
	}
}

// runNeighbors prints the node's neighbour table, as `ip neigh` would.
func runNeighbors(client janusv1alpha1.NetworkServiceClient, args []string) {
	fs := flag.NewFlagSet("network neighbors", flag.ExitOnError)
	iface := fs.String("interface", "", "only this interface's")
	parseAnywhere(fs, args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: janusctl network neighbors [-interface IF]")
		os.Exit(2)
	}
	c, cancel := ctx()
	defer cancel()
	resp, err := client.NeighborList(c, &janusv1alpha1.NeighborListRequest{Interface: *iface})
	check("NeighborList", err)
	printNeighbors(os.Stdout, resp.GetNeighbors())
}

func printNeighbors(out io.Writer, ns []*janusv1alpha1.Neighbor) {
	if len(ns) == 0 {
		fmt.Fprintln(out, "no neighbours")
		return
	}
	t := newTable(out)
	fmt.Fprintln(t, "INTERFACE\tADDRESS\tMAC\tSTATE\tCONFIRMED\tUSED")
	for _, n := range ns {
		state := strings.ToLower(strings.TrimPrefix(n.GetState().String(), "NEIGHBOR_STATE_"))
		if n.GetRouter() {
			state += " (router)"
		}
		mac := n.GetLinkAddress()
		if mac == "" {
			mac = "-"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", n.GetInterface(), n.GetAddress(), mac, state, ago(n.GetConfirmedMsAgo()), ago(n.GetUsedMsAgo()))
	}
	_ = t.Flush()
}

// ago reads a neighbour entry's age: "12s ago", to the second.
func ago(ms uint64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String() + " ago" //nolint:gosec // G115: ages the kernel counts in hundredths of a second fit
}

func runNetworkModules(client janusv1alpha1.NetworkServiceClient) {
	c, cancel := ctx()
	defer cancel()
	state := func(st janusv1alpha1.ModuleState, err error) string {
		if err != nil {
			return status.Convert(err).Message()
		}
		return strings.ToLower(strings.TrimPrefix(st.String(), "MODULE_STATE_"))
	}
	bgp, err := client.BGPStatus(c, &emptypb.Empty{})
	fmt.Println("bgp (bird):         ", state(bgp.GetState(), err))
	vrrp, err := client.VRRPStatus(c, &emptypb.Empty{})
	fmt.Println("vrrp (keepalived):  ", state(vrrp.GetState(), err))
	fw, err := client.FirewallList(c, &emptypb.Empty{})
	fmt.Println("firewall (nftables):", state(fw.GetState(), err))
	cs, err := client.ConsulStatus(c, &emptypb.Empty{})
	fmt.Println("consul:             ", state(cs.GetState(), err))
}
