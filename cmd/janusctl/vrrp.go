package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

var vrrpUsage = []string{
	"network vrrp status                VRRP (keepalived extension): each instance's state, interface, priority, virtual IPs",
	"network vrrp get                   the saved keepalived.conf",
	"network vrrp check FILE            have keepalived check a keepalived.conf, change nothing",
	"network vrrp apply FILE            check, save and reload keepalived.conf (an empty FILE stops keepalived)",
}

func runVRRP(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewNetworkServiceClient(conn)
	c, cancel := ctx()
	defer cancel()
	switch args[0] {
	case "status":
		resp, err := client.VRRPStatus(c, &emptypb.Empty{})
		check("VRRPStatus", err)
		fmt.Println("State:  ", strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")))
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			return
		}
		fmt.Println("Saved:  ", map[bool]string{true: "yes", false: "no"}[resp.GetConfigured()])
		fmt.Printf("HAProxy: %s (%s)\n", map[bool]string{true: "healthy", false: "not answering"}[resp.GetHaproxyHealthy()], resp.GetHaproxyHealthFile())
		if resp.GetError() != "" {
			fmt.Println("Error:  ", resp.GetError())
		}
		if len(resp.GetInstances()) > 0 {
			tw := table("INSTANCE", "STATE", "INTERFACE", "VRID", "PRIORITY", "VIRTUAL IPS", "SINCE")
			for _, in := range resp.GetInstances() {
				since := ""
				if in.GetLastTransitionUnix() > 0 {
					since = time.Unix(in.GetLastTransitionUnix(), 0).Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d/%d\t%s\t%s\n", in.GetName(), in.GetRole(), in.GetInterface(), in.GetVirtualRouterId(),
					in.GetEffectivePriority(), in.GetPriority(), strings.Join(in.GetVirtualIps(), " "), since)
			}
			tw.Flush()
		}
	case "get":
		resp, err := client.VRRPGetConfig(c, &emptypb.Empty{})
		check("VRRPGetConfig", err)
		if resp.GetIsDefault() {
			fmt.Fprintln(os.Stderr, "# no keepalived.conf saved")
		}
		os.Stdout.Write(resp.GetConfig())
	case "check", "apply":
		if len(args) != 2 {
			fmt.Fprintf(os.Stderr, "usage: janusctl network vrrp %s FILE\n", args[0])
			os.Exit(2)
		}
		cfg, err := os.ReadFile(args[1])
		check("read", err)
		resp, err := client.VRRPApplyConfig(c, &janusv1alpha1.VRRPApplyConfigRequest{Config: cfg, ValidateOnly: args[0] == "check"})
		check("VRRPApplyConfig", err)
		if !resp.GetAccepted() {
			printErrors(resp.GetErrors())
			os.Exit(1)
		}
		if args[0] == "check" {
			fmt.Println("valid")
		} else {
			fmt.Println("applied - keepalived reloads it")
		}
	default:
		fmt.Fprintf(os.Stderr, "janusctl network vrrp: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}
