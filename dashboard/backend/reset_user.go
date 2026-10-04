package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// resetUser is `dashboardd reset-user [-data-dir DIR] NAME`, run on the
// Controller's host (`docker exec janus-controller /dashboardd reset-user
// NAME`) when no admin can sign in: NAME gets a new password, printed
// once, to change at the next sign-in - enabled, and made an admin if it
// doesn't exist. A running Controller reads the change at once, and the
// account's sessions there end.
func resetUser(args []string) {
	fs := flag.NewFlagSet("reset-user", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/data", "the Controller's data directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: dashboardd reset-user [-data-dir DIR] NAME")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	name := fs.Arg(0)
	_, existed := mustOpenAuth(*dataDir).User(name)
	password, err := auth.ResetFromHost(*dataDir, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset-user:", err)
		os.Exit(1)
	}
	if !existed {
		fmt.Printf("Made the admin account %s.\n", name)
	}
	fmt.Printf("New password for %s: %s\nIt must be changed at the next sign-in; the account's sessions are over.\n", name, password)
}

func mustOpenAuth(dataDir string) *auth.Store {
	s, err := auth.Open(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset-user:", err)
		os.Exit(1)
	}
	return s
}
