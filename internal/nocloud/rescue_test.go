package nocloud

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRescueVolume: a rescue volume written as ISO9660 and as FAT is
// found among other volumes - a cidata one first - by its label, and
// read; FindVolume still takes the cidata one.
func TestRescueVolume(t *testing.T) {
	for _, fat := range []bool{false, true} {
		dir := t.TempDir()
		rescue := filepath.Join(dir, "rescue.img")
		if err := WriteVolume(rescue, dir, RescueLabel, fat, map[string][]byte{RescueFileName: []byte(`{"firewall":"skip"}`)}); err != nil {
			t.Fatalf("fat %v: %v", fat, err)
		}
		cidata := filepath.Join(dir, "cidata.img")
		if err := WriteVolume(cidata, dir, "cidata", fat, map[string][]byte{"user-data": []byte(`{}`), "meta-data": []byte("{}\n")}); err != nil {
			t.Fatal(err)
		}
		candidates := []string{filepath.Join(dir, "missing"), cidata, rescue}
		got, err := FindRescue(candidates)
		if err != nil || got != rescue {
			t.Fatalf("fat %v: FindRescue %q %v", fat, got, err)
		}
		r, err := ReadRescue(got)
		if err != nil || r.Firewall != "skip" {
			t.Fatalf("fat %v: ReadRescue %+v %v", fat, r, err)
		}
		if v, err := FindVolume(candidates); err != nil || v != cidata {
			t.Errorf("fat %v: FindVolume %q %v", fat, v, err)
		}
		if _, err := FindRescue([]string{cidata}); err != ErrNoRescue {
			t.Errorf("fat %v: no rescue volume: %v", fat, err)
		}
		if err := WriteVolume(rescue, dir, RescueLabel, fat, nil); err == nil || !strings.Contains(err.Error(), "exists already") {
			t.Errorf("fat %v: over an existing file: %v", fat, err)
		}
	}
}

func TestParseRescue(t *testing.T) {
	for raw, want := range map[string]string{
		`{}`:                    "",
		`{"firewall":"skip"}`:   "",
		`{"firewall":"off"}`:    `"skip" or nothing`,
		`{"firewall_off":true}`: "unknown field",
		`not json`:              "invalid character",
	} {
		_, err := ParseRescue([]byte(raw))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
	if err := WriteVolume(filepath.Join(t.TempDir(), "x.img"), t.TempDir(), "A-LABEL-TOO-LONG", true, nil); err == nil {
		t.Error("a FAT label of 16 characters")
	}
	if err := WriteVolume(filepath.Join(t.TempDir(), "x.img"), t.TempDir(), RescueLabel, false, map[string][]byte{"a/b": nil}); err == nil {
		t.Error("a file in a directory")
	}
}
