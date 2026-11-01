package firewall

import (
	"strings"
	"testing"
)

// A ruleset can't take the handoff's table name: janusd puts its own
// table of that name beside it.
func TestReservedTableName(t *testing.T) {
	for _, rs := range []string{
		"table ip janus_handoff {\n}\n",
		"table inet filter {\n}\ntable ip6 janus_handoff {\n}\n",
	} {
		errs, err := Check(rs)
		if err == nil || len(errs) != 1 || !strings.Contains(errs[0], "janus_handoff is janusd's own table name") {
			t.Errorf("%q: %v %v", rs, errs, err)
		}
	}
	if ReservedTables.MatchString("table ip janus_handoffs {}") {
		t.Error("another name taken for the reserved one")
	}
}
