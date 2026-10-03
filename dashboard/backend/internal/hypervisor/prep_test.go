package hypervisor

import "testing"

func TestPasteUnsafe(t *testing.T) {
	for script, unsafe := range map[string]bool{
		`echo "a!b"`:                    true,
		`if (!allowed) {`:               true,
		`echo 'user@pve!controller'`:    false,
		`if ! pvesh get /pools/x; then`: false,
		`if (a != b) {`:                 false,
		"# kvm!01\necho ok":             false,
		"ok\n    # comment!yes":         false,
	} {
		if got := PasteUnsafe(script) != ""; got != unsafe {
			t.Errorf("PasteUnsafe(%q) = %v, want %v", script, got, unsafe)
		}
	}
}
