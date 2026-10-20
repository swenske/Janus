package auth

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// Password hashes at bcrypt's minimum cost: these tests make and check
// dozens, each 70 ms at the default (more under the race detector).
func TestMain(m *testing.M) {
	SetBcryptCostForTests(bcrypt.MinCost)
	os.Exit(m.Run())
}
