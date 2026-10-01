package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"slices"
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
	stream, err := client.NetworkConfigApply(c, &janusv1alpha1.NetworkConfigApplyRequest{Config: cfg, ConfirmTimeoutSeconds: uint32(timeout.Seconds())})
	check("NetworkConfigApply", err)
	var addrs []string
	var revertAt time.Time
	applying := false
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			code := status.Code(err)
			if !applying || (code != codes.Unavailable && code != codes.DeadlineExceeded && code != codes.Canceled) {
				log.Fatalf("NetworkConfigApply: %v", err)
			}
			// Expected when the node dropped the address this call came
			// in on: the configuration is applied, just unreported.
			fmt.Printf("(connection lost while applying - expected if the node's address changed: %s)\n", status.Convert(err).Message())
			break
		}
		fmt.Printf("[%s] %s\n", resp.GetStage(), resp.GetMessage())
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
	if *noConfirm {
		fmt.Printf("not confirmed - run `janusctl network confirm` over an address the configuration keeps before %s, or it reverts\n", revertAt.Format(time.TimeOnly))
		return
	}

	candidates := confirmEndpoints(endpoint, cfg, addrs)
	deadline := revertAt.Add(-time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		for _, ep := range candidates {
			via, err := confirmOver(ep, redial)
			if err == nil {
				fmt.Printf("confirmed via %s (reached the node at %s) - the configuration is saved\n", via, ep)
				return
			}
			if attempt == 0 {
				fmt.Printf("confirm over %s: %v\n", ep, err)
			}
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("couldn't confirm over %s before %s - the node reverts to its previous configuration by itself", strings.Join(candidates, ", "), revertAt.Format(time.TimeOnly))
}

// confirmEndpoints lists where to reach the node after the change: the
// endpoint already in use first (it works whenever the configuration
// keeps that address), then every address the node reported, or - when
// the report was lost with the old address - the configuration's static
// ones.
func confirmEndpoints(endpoint string, cfg *janusv1alpha1.NetworkConfig, reported []string) []string {
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		port = "9505"
	}
	out := []string{endpoint}
	add := func(cidr string) {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || !p.Addr().IsGlobalUnicast() {
			return
		}
		ep := net.JoinHostPort(p.Addr().String(), port)
		if !slices.Contains(out, ep) {
			out = append(out, ep)
		}
	}
	for _, a := range reported {
		add(a)
	}
	if len(reported) == 0 {
		for _, iface := range cfg.GetInterfaces() {
			for _, a := range iface.GetAddresses() {
				add(a)
			}
		}
	}
	return out
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
		return "", errors.New(status.Convert(err).Message())
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
			fmt.Printf("  %-12s   dhcp (boot, not renewed): %s from %s, router %s, dns %s, ntp %s\n", "", l.GetAddress(), l.GetServer(), l.GetRouter(), strings.Join(l.GetDnsServers(), " "), strings.Join(l.GetNtpServers(), " "))
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
}
