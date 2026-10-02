package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

var haproxyFilesUsage = []string{
	"haproxy files                      HAProxy's own files (/etc/haproxy/files): error pages, maps, certificates haproxy.cfg references",
	"haproxy file-get NAME              print a file (never one holding a private key)",
	"haproxy file-put [-reload] NAME FILE  write a file - refused if haproxy.cfg wouldn't load with it; -reload: HAProxy uses it at once",
	"haproxy file-delete [-reload] NAME    remove a file - refused while haproxy.cfg needs it",
}

// runHAProxyFiles handles the file subcommands; false if sub isn't one.
func runHAProxyFiles(client janusv1alpha1.HAProxyServiceClient, sub string, args []string) bool {
	c, cancel := ctx()
	defer cancel()
	switch sub {
	case "files":
		resp, err := client.FileList(c, &emptypb.Empty{})
		check("FileList", err)
		fmt.Println("Directory:", resp.GetDir())
		tw := table("NAME", "SIZE", "MODIFIED", "", "SHA256")
		for _, f := range resp.GetFiles() {
			secret := ""
			if f.GetSecret() {
				secret = "private key"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.16s\n", f.GetName(), f.GetSize(), time.Unix(f.GetModifiedUnix(), 0).Format(time.RFC3339), secret, f.GetSha256())
		}
		tw.Flush()
	case "file-get":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy file-get NAME")
			os.Exit(2)
		}
		resp, err := client.FileGet(c, &janusv1alpha1.FileGetRequest{Name: args[0]})
		check("FileGet", err)
		os.Stdout.Write(resp.GetContent())
	case "file-put", "file-delete":
		fs := flag.NewFlagSet("haproxy "+sub, flag.ExitOnError)
		reload := fs.Bool("reload", false, "reload HAProxy afterwards")
		_ = fs.Parse(args)
		var accepted bool
		var errs []string
		if sub == "file-put" {
			if fs.NArg() != 2 {
				fmt.Fprintln(os.Stderr, "usage: janusctl haproxy file-put [-reload] NAME FILE")
				os.Exit(2)
			}
			data, err := os.ReadFile(fs.Arg(1))
			check("read", err)
			resp, err := client.FilePut(c, &janusv1alpha1.FilePutRequest{Name: fs.Arg(0), Content: data, Reload: *reload})
			check("FilePut", err)
			accepted, errs = resp.GetAccepted(), resp.GetErrors()
		} else {
			if fs.NArg() != 1 {
				fmt.Fprintln(os.Stderr, "usage: janusctl haproxy file-delete [-reload] NAME")
				os.Exit(2)
			}
			resp, err := client.FileDelete(c, &janusv1alpha1.FileDeleteRequest{Name: fs.Arg(0), Reload: *reload})
			check("FileDelete", err)
			accepted, errs = resp.GetAccepted(), resp.GetErrors()
		}
		if !accepted {
			fmt.Fprintln(os.Stderr, "refused: haproxy.cfg wouldn't load with this change")
			printErrors(errs)
			os.Exit(1)
		}
		if *reload {
			fmt.Println("done - HAProxy reloaded")
		} else {
			fmt.Println("done - HAProxy uses it from its next reload")
		}
	default:
		return false
	}
	return true
}
