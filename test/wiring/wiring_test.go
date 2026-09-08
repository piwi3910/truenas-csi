// Package wiring_test guards against code that is written, tested and never
// reached.
//
// This exists because that failure has now happened four times in this
// repository: the orphan reconciler, the node reachability probe (#9), the
// replication controller (#10), and pool administration and volume migration
// (#11). One of them made every dynamically provisioned volume unschedulable on
// a real cluster while the entire unit suite was green.
//
// Nothing else in the gate can see it:
//
//   - golangci-lint's `unused` reports only UNEXPORTED symbols. An exported
//     function in an internal package that nothing imports is invisible to it.
//   - Unit tests call these packages directly, so coverage looks healthy.
//   - `go build ./...` compiles an unimported package quite happily.
//
// The check is deliberately about IMPORTS rather than about calls: a package no
// binary can reach is either dead or unwired, and both are worth failing on.
package wiring_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// leafPackages are the packages allowed to have no non-test importer.
//
// Keep this list SHORT and justify every entry. An entry here is a promise that
// the package is reachable some other way, or deliberately not shipped yet —
// not a way to silence the check.
var leafPackages = map[string]string{
	"internal/driver": "the driver's identity constants; imported by name from " +
		"ldflags at build time as well as by code",
	"internal/truenas/fake": "an in-process appliance used by tests across many " +
		"packages; it is test infrastructure and is never meant to ship in a binary",
	"internal/truenas/core/fake": "the CORE flavour's equivalent, same reasoning",
}

// TestEveryInternalPackageIsReachable fails when a package under internal/ has
// no importer outside itself and outside tests.
func TestEveryInternalPackageIsReachable(t *testing.T) {
	root := repoRoot(t)

	const modulePrefix = "github.com/piwi3910/truenas-csi/"

	// Both platforms, unioned. `go list` resolves build tags for ONE GOOS, and
	// this repository has linux-only and darwin-only files -- so a package
	// imported only from a linux file would look unreachable when the check runs
	// on a developer's Mac, and the reverse on CI. Asking for both and taking
	// the union means a package counts as reachable if any supported build
	// reaches it, which is the honest question.
	var pkgs []goPackage
	imported := map[string]bool{}
	for _, goos := range []string{"linux", "darwin"} {
		for _, p := range listPackages(t, root, goos) {
			pkgs = append(pkgs, p)
			for _, imp := range p.Imports {
				imported[imp] = true
			}
		}
	}

	seen := map[string]bool{}
	var unreachable []string
	for _, p := range pkgs {
		if seen[p.ImportPath] {
			continue
		}
		seen[p.ImportPath] = true
		rel := strings.TrimPrefix(p.ImportPath, modulePrefix)
		if !strings.HasPrefix(rel, "internal/") {
			continue
		}
		if reason, ok := leafPackages[rel]; ok {
			t.Logf("allowed leaf %s: %s", rel, reason)
			continue
		}
		if !imported[p.ImportPath] {
			unreachable = append(unreachable, rel)
		}
	}
	sort.Strings(unreachable)

	for _, rel := range unreachable {
		t.Errorf("package %s has no non-test importer, so no binary can reach it.\n"+
			"Either wire it into cmd/, or, if it is deliberately not shipped yet, add it to "+
			"leafPackages with the reason. Code that is correct, tested and unreachable has "+
			"shipped from this repository four times; see issues #9, #10 and #11.", rel)
	}
	if len(unreachable) == 0 {
		t.Logf("all internal packages are reachable from a binary")
	}
}

// repoRoot returns the repository root, found by walking up to the go.mod that
// declares the driver module.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/piwi3910/truenas-csi\n") {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root")
	return ""
}

type goPackage struct {
	ImportPath string
	Imports    []string
}

// listPackages asks the toolchain for the module's packages and their imports.
//
// `go list` is used rather than parsing imports by hand because it resolves
// build tags the same way the real build does — this repository has linux-only
// and darwin-only files, and a hand parser would report a package as unimported
// on whichever platform CI happens to run.
func listPackages(t *testing.T, root, goos string) []goPackage {
	t.Helper()
	cmd := exec.Command("go", "list", "-e", "-json=ImportPath,Imports", "./...")
	cmd.Dir = root
	// GOWORK=off so the operator module's imports cannot mask an unreachable
	// driver package: the operator is built separately and must not be what
	// keeps a driver package alive.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOOS="+goos, "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list for GOOS=%s: %v", goos, err)
	}
	var pkgs []goPackage
	dec := newJSONStream(out)
	for {
		var p goPackage
		if !dec.next(&p) {
			break
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages")
	}
	return pkgs
}

// jsonStream decodes the concatenated JSON objects `go list -json` emits.
type jsonStream struct{ dec *json.Decoder }

func newJSONStream(b []byte) *jsonStream {
	return &jsonStream{dec: json.NewDecoder(bytes.NewReader(b))}
}

func (s *jsonStream) next(v any) bool { return s.dec.Decode(v) == nil }
