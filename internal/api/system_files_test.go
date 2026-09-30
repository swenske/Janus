package api

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fakeStream collects what a server-streaming RPC sends.
type fakeStream[T any] struct {
	grpc.ServerStream
	got []*T
}

func (f *fakeStream[T]) Send(m *T) error          { f.got = append(f.got, m); return nil }
func (f *fakeStream[T]) Context() context.Context { return context.Background() }

// tree builds root/{a.txt, sub/b.txt, sub/deeper/c.txt, link -> a.txt}.
func tree(t *testing.T) string {
	root := t.TempDir()
	for name, content := range map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo!", "sub/deeper/c.txt": "charlie"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	return root
}

func names(infos []*janusv1alpha1.FileInfo) string {
	var out []string
	for _, i := range infos {
		out = append(out, i.RelativeName)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestList(t *testing.T) {
	s, root := &System{}, tree(t)

	flat := &fakeStream[janusv1alpha1.FileInfo]{}
	if err := s.List(&janusv1alpha1.ListRequest{Root: root}, flat); err != nil {
		t.Fatal(err)
	}
	if got := names(flat.got); got != "a.txt,link,sub" {
		t.Errorf("List = %s", got)
	}

	deep := &fakeStream[janusv1alpha1.FileInfo]{}
	if err := s.List(&janusv1alpha1.ListRequest{Root: root, Recursive: true}, deep); err != nil {
		t.Fatal(err)
	}
	if got := names(deep.got); got != "a.txt,link,sub,sub/b.txt,sub/deeper,sub/deeper/c.txt" {
		t.Errorf("recursive List = %s", got)
	}
	for _, i := range deep.got {
		if i.RelativeName == "sub/b.txt" && (i.Size != 6 || i.IsDir) {
			t.Errorf("sub/b.txt = %+v", i)
		}
	}

	if err := s.List(&janusv1alpha1.ListRequest{Root: "relative"}, flat); status.Code(err) != codes.InvalidArgument {
		t.Errorf("relative root: %v", err)
	}
}

func readAll(t *testing.T, s *System, path string) (string, error) {
	st := &fakeStream[janusv1alpha1.Data]{}
	err := s.Read(&janusv1alpha1.ReadRequest{Path: path}, st)
	var b bytes.Buffer
	for _, d := range st.got {
		b.Write(d.Bytes)
	}
	return b.String(), err
}

func TestRead(t *testing.T) {
	s, root := &System{}, tree(t)
	if got, err := readAll(t, s, filepath.Join(root, "sub/deeper/c.txt")); err != nil || got != "charlie" {
		t.Errorf("Read = %q, %v", got, err)
	}
	big := filepath.Join(root, "big")
	want := strings.Repeat("0123456789", 20000) // spans several chunks
	if err := os.WriteFile(big, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readAll(t, s, big); err != nil || got != want {
		t.Errorf("Read(big) = %d bytes, %v", len(got), err)
	}
	if _, err := readAll(t, s, root); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Read(dir) = %v", err)
	}
	if _, err := readAll(t, s, "/dev/null"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Read(/dev/null) = %v", err)
	}
	if _, err := readAll(t, s, filepath.Join(root, "missing")); status.Code(err) != codes.NotFound {
		t.Errorf("Read(missing) = %v", err)
	}
}

func TestCopy(t *testing.T) {
	s, root := &System{}, tree(t)
	st := &fakeStream[janusv1alpha1.Data]{}
	if err := s.Copy(&janusv1alpha1.CopyRequest{RootPath: root}, st); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	for _, d := range st.got {
		archive.Write(d.Bytes)
	}
	base := filepath.Base(root)
	got := map[string]string{}
	tr := tar.NewReader(&archive)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("not a valid tar: %v", err)
		}
		content, _ := io.ReadAll(tr)
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			got[hdr.Name] = "-> " + hdr.Linkname
		case tar.TypeDir:
			got[hdr.Name] = "dir"
		default:
			got[hdr.Name] = string(content)
		}
	}
	want := map[string]string{
		base + "/": "dir", base + "/a.txt": "alpha", base + "/link": "-> a.txt",
		base + "/sub/": "dir", base + "/sub/b.txt": "bravo!",
		base + "/sub/deeper/": "dir", base + "/sub/deeper/c.txt": "charlie",
	}
	if len(got) != len(want) {
		t.Errorf("archive = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("archive[%q] = %q, want %q", k, got[k], v)
		}
	}

	if err := s.Copy(&janusv1alpha1.CopyRequest{RootPath: "/proc/self"}, st); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Copy(/proc/self) = %v", err)
	}
}

func TestDiskUsage(t *testing.T) {
	root := tree(t)
	got, err := diskUsage(root, false)
	if err != nil || len(got) != 1 || got[0].SizeBytes != int64(len("alpha")+len("bravo!")+len("charlie")) {
		t.Fatalf("diskUsage = %v, %v", got, err)
	}
	perDir, err := diskUsage(root, true)
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, e := range perDir {
		rel, _ := filepath.Rel(root, e.Path)
		sizes[rel] = e.SizeBytes
	}
	if sizes["."] != 18 || sizes["sub"] != 13 || sizes["sub/deeper"] != 7 || len(sizes) != 3 {
		t.Errorf("per-dir sizes = %v", sizes)
	}
	if last := perDir[len(perDir)-1]; last.Path != root {
		t.Errorf("root should come last, got %v", last.Path)
	}
	file, err := diskUsage(filepath.Join(root, "a.txt"), false)
	if err != nil || file[0].SizeBytes != 5 || file[0].IsDir {
		t.Errorf("diskUsage(file) = %v, %v", file, err)
	}
}
