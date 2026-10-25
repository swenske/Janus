package libvirt

import "testing"

// The host's identity is the capabilities' <host><uuid> - not a guest's
// or a NUMA cell's.
func TestCapsHostUUID(t *testing.T) {
	caps := `<capabilities>
  <host>
    <uuid>4C4C4544-0042-3510-8051-B7C04F4B4E32</uuid>
    <cpu><arch>x86_64</arch></cpu>
    <topology><cells num='1'><cell id='0'><memory unit='KiB'>32768000</memory></cell></cells></topology>
  </host>
  <guest><os_type>hvm</os_type></guest>
</capabilities>`
	if got := capsHostUUID(caps); got != "4c4c4544-0042-3510-8051-b7c04f4b4e32" {
		t.Errorf("got %q", got)
	}
	if got := capsHostUUID("<capabilities><host></host></capabilities>"); got != "" {
		t.Errorf("no uuid: %q", got)
	}
	if got := capsHostUUID("not xml"); got != "" {
		t.Errorf("not XML: %q", got)
	}
}
