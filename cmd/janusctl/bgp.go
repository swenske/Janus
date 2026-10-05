package main

import (
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

func runBGP(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewNetworkServiceClient(conn)
	c, cancel := ctx()
	defer cancel()
	switch args[0] {
	case "status":
		resp, err := client.BGPStatus(c, &emptypb.Empty{})
		check("BGPStatus", err)
		fmt.Println("State:    ", strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")))
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			return
		}
		fmt.Println("Saved:    ", map[bool]string{true: "yes", false: "no"}[resp.GetConfigured()])
		fmt.Println("HAProxy:  ", map[bool]string{true: "healthy", false: "not answering - the haproxy_* protocols are held down"}[resp.GetHaproxyHealthy()])
		if resp.GetVersion() != "" {
			fmt.Printf("BIRD:      %s, router ID %s\n", resp.GetVersion(), resp.GetRouterId())
		}
		if resp.GetError() != "" {
			fmt.Println("Error:    ", resp.GetError())
		}
		if len(resp.GetProtocols()) > 0 {
			tw := table("PROTOCOL", "TYPE", "STATE", "SINCE", "INFO", "NEIGHBOR", "ROUTES (IN/OUT)")
			for _, p := range resp.GetProtocols() {
				info := p.GetInfo()
				if p.GetHeldDown() {
					info = strings.TrimSpace(info + " (held down: HAProxy doesn't answer)")
				}
				if p.GetLastError() != "" && !strings.Contains(info, p.GetLastError()) {
					info = strings.TrimSpace(info + " - " + p.GetLastError())
				}
				neighbor := ""
				if p.GetNeighborAddress() != "" {
					neighbor = fmt.Sprintf("%s AS%d", p.GetNeighborAddress(), p.GetNeighborAs())
				}
				var routes []string
				for _, ch := range p.GetChannels() {
					routes = append(routes, fmt.Sprintf("%s %d/%d", ch.GetName(), ch.GetImported(), ch.GetExported()))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.GetName(), p.GetProtocol(), p.GetState(), p.GetSince(), info, neighbor, strings.Join(routes, " "))
			}
			tw.Flush()
		}
	case "get":
		resp, err := client.BGPGetConfig(c, &emptypb.Empty{})
		check("BGPGetConfig", err)
		if resp.GetIsDefault() {
			fmt.Fprintln(os.Stderr, "# no bird.conf saved")
		}
		os.Stdout.Write(resp.GetConfig())
	case "check", "apply":
		if len(args) != 2 {
			fmt.Fprintf(os.Stderr, "usage: janusctl network bgp %s FILE\n", args[0])
			os.Exit(2)
		}
		cfg, err := os.ReadFile(args[1])
		check("read", err)
		resp, err := client.BGPApplyConfig(c, &janusv1alpha1.BGPApplyConfigRequest{Config: cfg, ValidateOnly: args[0] == "check"})
		check("BGPApplyConfig", err)
		if !resp.GetAccepted() {
			printErrors(resp.GetErrors())
			os.Exit(1)
		}
		if args[0] == "check" {
			fmt.Println("valid")
		} else {
			fmt.Println("applied - BIRD reconfigures")
		}
	default:
		fmt.Fprintf(os.Stderr, "janusctl network bgp: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}
