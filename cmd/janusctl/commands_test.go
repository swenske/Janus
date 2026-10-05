package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The command tree (commands.go) and the code dispatching the commands
// are written apart: these tests read the code and fail when they part
// - a command or a flag the tree doesn't know (no help, no completion),
// or one it offers that doesn't exist.

// parsePackage is this package's code, tests left out.
func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			f, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	}
	return files
}

func funcs(files []*ast.File) map[string]*ast.FuncDecl {
	out := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil && fn.Recv == nil {
				out[fn.Name.Name] = fn
			}
		}
	}
	return out
}

func lit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

// caseLabels are the string labels of the case clauses of sw.
func caseLabels(sw *ast.SwitchStmt) []string {
	var out []string
	for _, st := range sw.Body.List {
		for _, e := range st.(*ast.CaseClause).List {
			if s, ok := lit(e); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// flagSets maps each FlagSet's name (its command's path) to its flags.
// A name made of a prefix and the command word ("network consul " +
// args[0]) stands for every label of the case clause it's made in.
func flagSets(files []*ast.File) map[string][]string {
	out := map[string][]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			sets := map[string][]string{} // variable -> names
			var clause []string
			var visit func(n ast.Node) bool
			visit = func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CaseClause:
					saved := clause
					clause = nil
					for _, e := range n.List {
						if s, ok := lit(e); ok {
							clause = append(clause, s)
						}
					}
					for _, st := range n.Body {
						ast.Inspect(st, visit)
					}
					clause = saved
					return false
				case *ast.AssignStmt:
					if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
						return true
					}
					call, ok := n.Rhs[0].(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "NewFlagSet" {
						return true
					}
					var names []string
					if s, ok := lit(call.Args[0]); ok {
						names = []string{s}
					} else if bin, ok := call.Args[0].(*ast.BinaryExpr); ok {
						prefix, _ := lit(bin.X)
						for _, c := range clause {
							names = append(names, prefix+c)
						}
					}
					id := n.Lhs[0].(*ast.Ident).Name
					sets[id] = names
					for _, name := range names {
						if _, ok := out[name]; !ok {
							out[name] = []string{}
						}
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					id, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					names, isSet := sets[id.Name]
					if id.Name == "flag" && fn.Name.Name == "main" {
						names, isSet = []string{""}, true
					}
					if !isSet {
						return true
					}
					idx := 0
					switch sel.Sel.Name {
					case "String", "Bool", "Int", "Int64", "Uint", "Uint64", "Duration", "Float64", "Func":
					case "StringVar", "BoolVar", "IntVar", "UintVar", "DurationVar", "Var":
						idx = 1
					default:
						return true
					}
					if len(n.Args) <= idx {
						return true
					}
					if name, ok := lit(n.Args[idx]); ok {
						for _, set := range names {
							out[set] = append(out[set], name)
						}
					}
				}
				return true
			}
			ast.Inspect(fn.Body, visit)
		}
	}
	return out
}

func TestTreeFlagsMatchTheCode(t *testing.T) {
	sets := flagSets(parsePackage(t))
	var global []string
	for _, g := range globalFlags {
		global = append(global, g.name)
	}
	if got := sets[""]; !sameSet(got, global) {
		t.Errorf("global flags: the code has %v, the tree %v", sorted(got), sorted(global))
	}
	delete(sets, "")
	treeFlags := map[string][]string{}
	commands.leaves(nil, func(path []string, c *command) {
		if len(c.flags) == 0 {
			return
		}
		var names []string
		for _, fl := range c.flags {
			names = append(names, fl.name)
		}
		treeFlags[strings.Join(path, " ")] = names
	})
	for name, flags := range sets {
		if c, rest := commands.find(strings.Fields(name)); len(rest) > 0 || c == commands {
			t.Errorf("the code has a command %q the tree doesn't know", name)
			continue
		}
		if !sameSet(flags, treeFlags[name]) {
			t.Errorf("%s: the code's flags are %v, the tree's %v", name, sorted(flags), sorted(treeFlags[name]))
		}
	}
	for name := range treeFlags {
		if _, ok := sets[name]; !ok {
			t.Errorf("the tree gives %q flags the code doesn't parse", name)
		}
	}
}

// TestTreeCommandsMatchTheDispatch: each group's commands are exactly
// those its run function dispatches.
func TestTreeCommandsMatchTheDispatch(t *testing.T) {
	fns := funcs(parsePackage(t))
	labels := func(fn string, tag func(ast.Expr) bool) []string {
		var out []string
		ast.Inspect(fns[fn].Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SwitchStmt:
				// A switch inside a command's case is its arguments'
				// ("system service start|stop|restart"), not commands.
				if n.Tag != nil && tag(n.Tag) {
					out = append(out, caseLabels(n)...)
					return false
				}
			case *ast.BinaryExpr:
				// runSystem's args[0] == "pcap".
				if n.Op == token.EQL || n.Op == token.NEQ {
					if s, ok := lit(n.Y); ok && isArg0(n.X) {
						out = append(out, s)
					}
				}
			}
			return true
		})
		return out
	}
	arg0 := func(e ast.Expr) bool { return isArg0(e) || isIdent(e, "cmd", "sub") }
	arg1 := func(e ast.Expr) bool { return isIndex(e, "args", "1") }
	dispatch := map[string][]string{
		"":                 labels("main", arg0),
		"system":           append(labels("runSystem", arg0), labels("runSystemCommand", arg0)...),
		"haproxy":          append(labels("runHAProxy", arg0), labels("runHAProxyFiles", arg0)...),
		"haproxy acme":     labels("runACME", arg0),
		"network":          labels("runNetwork", arg0),
		"network firewall": labels("runFirewall", arg0),
		"network vrrp":     labels("runVRRP", arg0),
		"network bgp":      labels("runBGP", arg0),
		"network consul":   labels("runConsul", arg0),
		"access":           labels("runAccess", arg0),
		"pki":              labels("runPKI", arg0),
		"lifecycle":        labels("runLifecycle", arg0),
		"image":            labels("runImage", arg0),
		"fleet":            labels("runFleet", func(e ast.Expr) bool { return isIndex(e, "args", "0") }),
		"fleet issuer":     labels("runFleet", arg1),
	}
	for path, got := range dispatch {
		c, rest := commands.find(strings.Fields(path))
		if len(rest) > 0 {
			t.Errorf("no group %q in the tree", path)
			continue
		}
		var tree []string
		for _, s := range c.subs {
			tree = append(tree, s.name)
		}
		if !sameSet(got, tree) {
			t.Errorf("janusctl %s: the code dispatches %v, the tree has %v", path, sorted(got), sorted(tree))
		}
	}
}

func isArg0(e ast.Expr) bool {
	if isIndex(e, "args", "0") {
		return true
	}
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Arg" || !isIdent(sel.X, "flag") {
		return false
	}
	b, ok := call.Args[0].(*ast.BasicLit)
	return ok && b.Value == "0"
}

func isIndex(e ast.Expr, name, index string) bool {
	ix, ok := e.(*ast.IndexExpr)
	if !ok || !isIdent(ix.X, name) {
		return false
	}
	b, ok := ix.Index.(*ast.BasicLit)
	return ok && b.Value == index
}

func isIdent(e ast.Expr, names ...string) bool {
	id, ok := e.(*ast.Ident)
	return ok && slices.Contains(names, id.Name)
}

func sameSet(a, b []string) bool {
	return slices.Equal(sorted(a), sorted(b))
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return slices.Compact(out)
}

// TestTreeIsSound: what the pickers and completion rely on.
func TestTreeIsSound(t *testing.T) {
	commands.leaves(nil, func(path []string, c *command) {
		name := strings.Join(path, " ")
		if c.help == "" {
			t.Errorf("%s: no help", name)
		}
		if len(c.pos) > 0 && c.required > len(c.pos) && !c.variadic {
			t.Errorf("%s: %d required arguments, %d kinds", name, c.required, len(c.pos))
		}
		seen := map[string]bool{}
		for _, fl := range c.flags {
			if seen[fl.name] {
				t.Errorf("%s: -%s twice", name, fl.name)
			}
			seen[fl.name] = true
		}
	})
}
