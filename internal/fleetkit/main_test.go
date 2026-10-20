package fleetkit

import (
	"os"
	"testing"
)

// Kits at a work factor that costs milliseconds (3 s each at age's
// default under the race detector). TestDefaultWorkFactor makes the one
// kit at the default.
func TestMain(m *testing.M) {
	SetWorkFactorForTests(10)
	os.Exit(m.Run())
}
