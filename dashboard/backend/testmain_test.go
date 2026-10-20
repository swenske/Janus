package main

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/backup"
	"github.com/swenske/Janus/internal/fleetkit"
)

// The kits and the password hashes these tests make cost milliseconds,
// not seconds: a kit at age's default work factor took 3 s under the
// race detector, TestRestore makes ten - 254 s for this package.
func TestMain(m *testing.M) {
	fleetkit.SetWorkFactorForTests(10)
	backup.SetKitWorkFactorForTests(10)
	auth.SetBcryptCostForTests(bcrypt.MinCost)
	os.Exit(m.Run())
}
