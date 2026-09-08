package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestMainWiresEverySubsystemThatHasBeenOrphaned fails if main stops calling a
// starter that was, at some point, shipped with no caller at all.
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
func TestMainWiresEverySubsystemThatHasBeenOrphaned(t *testing.T) {
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
		// The replication reconciler was shipped for weeks with no caller: a
		// StorageProtectionGroup could be applied and nothing would ever
		// reconcile it (#10). test/wiring catches the package being unimported;
		// this catches the starter being imported but not called.
		"startReplication": false,
		// pooladmin and migration shipped with no binary reaching them at all
		// (#11): documented features that could not be invoked.
		"dispatchSubcommand": false,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			// A plain call: startReplication(...). Checked FIRST, because an
			// earlier version tested the selector case and returned before ever
			// reaching this one, so it could not see a package-local starter at
			// all -- a guard with a blind spot exactly where the bug lives.
			if _, tracked := want[fun.Name]; tracked {
				want[fun.Name] = true
			}
		case *ast.SelectorExpr:
			name := fun.Sel.Name
			if ident, ok := fun.X.(*ast.Ident); ok {
				name = ident.Name + "." + fun.Sel.Name
			}
			if _, tracked := want[name]; tracked {
				want[name] = true
			}
			if strings.HasSuffix(name, ".SetReachability") {
				want["SetReachability"] = true
			}
		}
		return true
	})

	for call, found := range want {
		if !found {
			t.Errorf("%s never calls %s.\n"+
				"Each of these was, at some point, correct code that no binary reached: "+
				"the reachability probe made every PVC fail to schedule on node affinity, "+
				"and the replication starter left StorageProtectionGroups unreconciled "+
				"with no error anywhere.", file, call)
		}
	}
}
