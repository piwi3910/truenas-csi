package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNodeModeWiresTheReachabilityProbe fails if the node branch of main stops
// calling ProbeReachability and SetReachability.
//
// It reads this package's own source on purpose. Every other kind of test in
// this repository failed to catch the bug it guards: Reachability.TopologyLabels
// was correct and covered, GetInfo merged it correctly, and the controller and
// node agreed on the label spelling -- and none of that mattered, because
// nothing in the built binary ever called SetReachability. n.reach stayed nil,
// nodes advertised no backend segment, and every dynamically provisioned volume
// bound and then failed to schedule with
//
//	0/8 nodes are available: 8 node(s) didn't match PersistentVolume's node affinity
//
// on a cluster whose entire unit suite was green.
//
// A test that exercises a function proves the function works. Proving it is
// REACHED means asserting on the call site, and the call site is here.
func TestNodeModeWiresTheReachabilityProbe(t *testing.T) {
	const file = "main.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	want := map[string]bool{
		"node.ProbeReachability": false,
		"SetReachability":        false,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if ident, ok := sel.X.(*ast.Ident); ok {
			name = ident.Name + "." + sel.Sel.Name
		}
		if _, tracked := want[name]; tracked {
			want[name] = true
		}
		if name == "nn.SetReachability" || strings.HasSuffix(name, ".SetReachability") {
			want["SetReachability"] = true
		}
		return true
	})

	for call, found := range want {
		if !found {
			t.Errorf("%s never calls %s.\n"+
				"Without it the node plugin advertises no reachability segment, the "+
				"controller requires one for every volume on a backend, and every PVC "+
				"binds and then fails to schedule on node affinity.", file, call)
		}
	}
}
