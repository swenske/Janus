package rbac

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/pki"
)

var updateReference = flag.Bool("update", false, "rewrite docs/private-cloud/api-reference.md")

// TestAPIReferenceDoc keeps the node API's reference page what the
// .proto files and this package say: every call, its request and
// response, the role and the domain it needs, its comment.
func TestAPIReferenceDoc(t *testing.T) {
	const file = "../../docs/private-cloud/api-reference.md"
	var b bytes.Buffer
	if err := writeAPIReference(&b, "../../api/proto/janus/v1alpha1"); err != nil {
		t.Fatal(err)
	}
	if *updateReference {
		if err := os.WriteFile(file, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, b.Bytes()) {
		t.Fatalf("docs/private-cloud/api-reference.md isn't what api/proto and internal/rbac say: go test ./internal/rbac -run TestAPIReferenceDoc -update")
	}
}

// The services, in the order the page shows them, and their files.
var referenceServices = []struct{ name, file string }{
	{"HAProxyService", "haproxy.proto"},
	{"NetworkService", "network.proto"},
	{"SystemService", "system.proto"},
	{"LifecycleService", "lifecycle.proto"},
	{"AccessService", "access.proto"},
}

type protoCall struct {
	name, request, response string
	comment                 []string
}

// protoGroup is a run of calls without a blank line between them, and
// the comment above them that no call takes (a detached comment).
type protoGroup struct {
	intro []string
	calls []protoCall
}

var rpcLine = regexp.MustCompile(`^rpc (\w+)\((stream )?([\w.]+)\) returns \((stream )?([\w.]+)\);$`)

// parseService reads service name's comment and its calls, grouped,
// from a .proto file.
func parseService(src, name string) (comment []string, groups []protoGroup, err error) {
	lines := strings.Split(src, "\n")
	start := slices.Index(lines, "service "+name+" {")
	if start < 0 {
		return nil, nil, fmt.Errorf("no service %s", name)
	}
	for i := start - 1; i >= 0 && strings.HasPrefix(lines[i], "//"); i-- {
		comment = append([]string{commentText(lines[i])}, comment...)
	}
	var pending []string
	group := protoGroup{}
	flush := func() {
		if len(group.calls) > 0 || len(group.intro) > 0 {
			groups = append(groups, group)
		}
		group = protoGroup{}
	}
	for _, line := range lines[start+1:] {
		line = strings.TrimSpace(line)
		switch {
		case line == "}":
			flush()
			return comment, groups, nil
		case strings.HasPrefix(line, "//"):
			pending = append(pending, commentText(line))
		case line == "":
			flush()
			group.intro, pending = pending, nil
		default:
			m := rpcLine.FindStringSubmatch(line)
			if m == nil {
				return nil, nil, fmt.Errorf("%s: can't read %q", name, line)
			}
			group.calls = append(group.calls, protoCall{
				name:     m[1],
				request:  m[2] + shortType(m[3]),
				response: m[4] + shortType(m[5]),
				comment:  pending,
			})
			pending = nil
		}
	}
	return nil, nil, fmt.Errorf("service %s doesn't end", name)
}

func commentText(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "//"))
}

func shortType(t string) string {
	return strings.TrimPrefix(t, "google.protobuf.")
}

var docsPath = regexp.MustCompile(`docs/([\w/-]+\.md)`)

// prose joins comment lines into Markdown: docs/... paths become links
// from docs/private-cloud/, and what a table cell or HTML would take for
// its own is escaped.
func prose(lines []string) string {
	s := strings.Join(lines, " ")
	s = strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;").Replace(s)
	return docsPath.ReplaceAllString(s, "[docs/$1](../$1)")
}

// lowestRole is the least role that may call method, as the page names
// it.
func lowestRole(method string) string {
	roles := RolesFor(method)
	for _, r := range []string{pki.RoleReader, pki.RoleOperator, pki.RoleAdmin} {
		if slices.Contains(roles, r) {
			return "`" + r + "`"
		}
	}
	return "-"
}

func writeAPIReference(w *bytes.Buffer, protoDir string) error {
	fmt.Fprint(w, `<!-- Generated from api/proto and internal/rbac: go test ./internal/rbac -run TestAPIReferenceDoc -update -->

# Node API reference

Every call of a Janus node's gRPC API, the role and the domain it needs,
and what it does - generated from the .proto files and the roles the
node enforces. The messages' fields are in the .proto files themselves
(`+"`api/proto/janus/v1alpha1`"+`, at the release your nodes run).

- **Role**: the least role that may make the call - `+"`os:operator`"+` may do
  what `+"`os:reader`"+` may, `+"`os:admin`"+` everything
  ([certificates and roles](orchestrator-certificates.md#roles)).
- **Domain**: what the call is about, for a permission narrowed to some
  domains (`+"`janus-as-domains`"+`); `+"`observe`"+` comes with any.
- A call the role or the domain doesn't allow answers `+"`PermissionDenied`"+`;
  a call to an extension the image doesn't have, `+"`FailedPrecondition`"+`.
- Every call is `+"`janus.v1alpha1.<Service>/<Call>`"+` - `+"`janus.v1alpha1.HAProxyService/ApplyConfig`"+`.
`)
	for _, svc := range referenceServices {
		src, err := os.ReadFile(protoDir + "/" + svc.file)
		if err != nil {
			return err
		}
		comment, groups, err := parseService(string(src), svc.name)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n## %s\n", svc.name)
		if len(comment) > 0 {
			fmt.Fprintf(w, "\n%s\n", prose(comment))
		}
		// A table goes on until a group's introduction.
		inTable := false
		for _, g := range groups {
			if len(g.intro) > 0 {
				fmt.Fprintf(w, "\n%s\n", prose(g.intro))
				inTable = false
			}
			if len(g.calls) == 0 {
				continue
			}
			if !inTable {
				fmt.Fprint(w, "\n| Call | Request → response | Role | Domain | What it does |\n|---|---|---|---|---|\n")
				inTable = true
			}
			for _, c := range g.calls {
				method := "/janus.v1alpha1." + svc.name + "/" + c.name
				if _, ok := Required[method]; !ok {
					return fmt.Errorf("%s has no role in Required", method)
				}
				fmt.Fprintf(w, "| `%s` | `%s` → `%s` | %s | %s | %s |\n",
					c.name, c.request, c.response, lowestRole(method), DomainOf(method), prose(c.comment))
			}
		}
	}
	return nil
}
