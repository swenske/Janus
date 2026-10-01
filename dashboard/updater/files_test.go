package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetEnvVar(t *testing.T) {
	const v = "JANUS_CONTROLLER_IMAGE"
	for _, tc := range []struct{ in, want string }{
		{"", v + "=x\n"},
		{"A=1", "A=1\n" + v + "=x\n"},
		{"A=1\n", "A=1\n" + v + "=x\n"},
		{"# comment\n" + v + "=old\nB=2\n", "# comment\n" + v + "=x\nB=2\n"},
		{"export " + v + " = old\n", v + "=x\n"},
		{"#" + v + "=commented\n", "#" + v + "=commented\n" + v + "=x\n"},
		{v + "_OTHER=1\n", v + "_OTHER=1\n" + v + "=x\n"},
		{v + "=a\n" + v + "=b\n", v + "=x\n" + v + "=x\n"},
		{v + "=old", v + "=x\n"},
	} {
		if got := string(setEnvVar([]byte(tc.in), v, "x")); got != tc.want {
			t.Errorf("setEnvVar(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCopyAndReplace(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	mustMkdir(t, filepath.Join(src, "nodes", "a"), 0o700)
	mustWrite(t, filepath.Join(src, "auth.json"), "secret", 0o600)
	mustWrite(t, filepath.Join(src, "nodes", "a", "meta.json"), "{}", 0o644)
	if err := os.Symlink("auth.json", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(root, "dst")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "nodes", "a")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("nodes/a: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "auth.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("auth.json: %v %v", fi, err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "link")); err != nil || link != "auth.json" {
		t.Errorf("link = %q, %v", link, err)
	}

	// The live directory changes; replacing its contents brings back
	// exactly the copy, the directory itself staying (a mount point).
	mustWrite(t, filepath.Join(src, "auth.json"), "changed", 0o600)
	mustWrite(t, filepath.Join(src, "new"), "new", 0o600)
	if err := os.RemoveAll(filepath.Join(src, "nodes")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(src)
	if err := replaceContents(src, dst); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(src)
	if !os.SameFile(before, after) {
		t.Error("the directory itself was replaced")
	}
	if got := readFile(t, filepath.Join(src, "auth.json")); got != "secret" {
		t.Errorf("auth.json = %q", got)
	}
	if _, err := os.Stat(filepath.Join(src, "new")); err == nil {
		t.Error("a file the copy doesn't have is still there")
	}
	if got := readFile(t, filepath.Join(src, "nodes", "a", "meta.json")); got != "{}" {
		t.Errorf("nodes/a/meta.json = %q", got)
	}
}

func TestWriteFileAtomicKeepsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := writeFileAtomic(path, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("new file mode = %v", fi.Mode().Perm())
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("A=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("rewritten file mode = %v, want kept at 0600", fi.Mode().Perm())
	}
	if got := readFile(t, path); got != "A=2\n" {
		t.Errorf("content = %q", got)
	}
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
}
