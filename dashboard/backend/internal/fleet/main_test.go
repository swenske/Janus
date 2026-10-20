package fleet

import (
	"os"
	"testing"

	"github.com/swenske/Janus/internal/fleetkit"
)

// Kits at a work factor that costs milliseconds (3 s each at age's
// default under the race detector).
func TestMain(m *testing.M) {
	fleetkit.SetWorkFactorForTests(10)
	os.Exit(m.Run())
}
