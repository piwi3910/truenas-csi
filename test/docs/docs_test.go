// Package docs_test guards the operator documentation against the code drifting
// away from it. Documentation is the only interface an operator has to the
// StorageClass parameters and to the TrueNAS privileges the driver needs, so a
// parameter added in code without a line in README.md is a defect, not a
// cosmetic omission.
package docs_test

import (
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/backend/iscsi"
	"github.com/piwi3910/truenas-csi/internal/backend/nfs"
	"github.com/piwi3910/truenas-csi/internal/backend/nvme"
	"github.com/piwi3910/truenas-csi/internal/backend/smb"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory (test/docs) to the module
// root, so the tests do not depend on where `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

func readDoc(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s not found: %v", rel, err)
	}
	return string(b)
}

// paramSourceDirs are the packages that read StorageClass parameters. The
// protocol backends land here as they are implemented; a directory that does not
// exist yet is skipped rather than failing, but at least one must exist or the
// test is not testing anything.
var paramSourceDirs = []string{
	filepath.Join("internal", "backend"),
	filepath.Join("internal", "backend", "nfs"),
	filepath.Join("internal", "backend", "iscsi"),
	filepath.Join("internal", "csi"),
}

// paramKeyRE matches a StorageClass parameter lookup: the parameter map is
// indexed by a string literal, e.g. params["fsType"] or r.Params["sparse"].
var paramKeyREs = []*regexp.Regexp{
	// params["fsType"] / r.Params["sparse"] / Parameters["backend"]
	regexp.MustCompile(`(?:[Pp]arams|Parameters)\[\s*"([A-Za-z][A-Za-z0-9]*)"\s*\]`),
	// ParamServer = "server" — the constant style used by the nfs backend
	regexp.MustCompile(`Param[A-Za-z0-9]*\s*=\s*"([A-Za-z][A-Za-z0-9]*)"`),
	// boolParam(m, "chap", true) — the helper style used by the iscsi backend
	regexp.MustCompile(`[A-Za-z]*Param\(\s*[A-Za-z_][A-Za-z0-9_]*\s*,\s*"([A-Za-z][A-Za-z0-9]*)"`),
	// m["portalID"] — direct indexing of a parameter map
	regexp.MustCompile(`\bm\[\s*"([A-Za-z][A-Za-z0-9]*)"\s*\]`),
}

// documentedParams is every parameter the driver defines, as the chart's
// StorageClass template and the spec list them. It is stated here as well as
// discovered from the sources so that README.md must document a parameter even
// while its backend is still landing.
var documentedParams = []string{
	"server",
	"nodeIQNs",
	"backend", "protocol", "pool", "parentDataset",
	"fsType", "sparse", "volblocksize",
	"nfsVersion", "networks", "maproot", "mode", "uid", "gid",
	"portalID", "chap", "initiatorACL", "multipath",
}

// paramsFromSources collects the parameter keys the code actually reads,
// mapping each to the file that reads it so a failure names the offender.
func paramsFromSources(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	dirsSeen := 0

	for _, rel := range paramSourceDirs {
		dir := filepath.Join(root, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				t.Logf("skipping %s: not implemented yet", rel)
				continue
			}
			t.Fatalf("read %s: %v", rel, err)
		}
		dirsSeen++
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for _, key := range findAllParamKeys(string(b)) {
				if _, ok := found[key]; !ok {
					found[key] = filepath.Join(rel, name)
				}
			}
		}
	}

	if dirsSeen == 0 {
		t.Skip("no parameter-reading source directories exist yet")
	}
	return found
}

// TestDocsListAllStorageClassParameters fails when a StorageClass parameter
// exists in the code but not in README.md. It is the check that keeps the
// parameter reference honest as backends are added.
func TestDocsListAllStorageClassParameters(t *testing.T) {
	root := repoRoot(t)
	readme := readDoc(t, root, "README.md")

	want := map[string]string{}
	for _, p := range documentedParams {
		want[p] = "the documented parameter set"
	}
	for p, where := range paramsFromSources(t, root) {
		want[p] = where
	}

	names := make([]string, 0, len(want))
	for p := range want {
		names = append(names, p)
	}
	sort.Strings(names)

	for _, p := range names {
		// The reference table renders each parameter in backticks; a bare
		// mention in prose is not documentation of the parameter.
		if !strings.Contains(readme, "`"+p+"`") {
			t.Errorf("StorageClass parameter %q (read in %s) is not documented in README.md", p, want[p])
		}
	}
}

// roles is the least-privilege set measured against the live appliance: an
// account holding exactly these, and nothing else, runs the whole
// test/integration suite green. Every name must appear in docs/security.md,
// because an operator copies this list into a TrueNAS privilege and a missing
// role means a driver call fails in production.
//
// It was NOT measured before, and two entries were wrong in ways that fail far
// from their cause. SHARING_NVME_TARGET_WRITE was absent, so nvmet.global.config
// returned EACCES and no NVMe volume could be created. And the set listed
// SHARING_ISCSI_AUTH_READ, which is worse than missing: TrueNAS answers the
// query with the CHAP secret MASKED instead of refusing it, so every iSCSI
// volume provisioned, every claim bound, and every attach then failed on the
// node with an authorization failure.
var roles = []string{
	"DATASET_WRITE",
	"DATASET_DELETE",
	"POOL_READ",
	"SNAPSHOT_WRITE",
	"SNAPSHOT_DELETE",
	"SHARING_ISCSI_EXTENT_WRITE",
	"SHARING_ISCSI_TARGET_WRITE",
	"SHARING_ISCSI_TARGETEXTENT_WRITE",
	"SHARING_ISCSI_GLOBAL_READ",
	"SHARING_ISCSI_PORTAL_READ",
	"SHARING_ISCSI_INITIATOR_READ",
	"SHARING_ISCSI_AUTH_WRITE",
	"SHARING_NFS_WRITE",
	"FILESYSTEM_ATTRS_WRITE",
	"SHARING_NVME_TARGET_WRITE",
}

// TestSecurityDocListsAllRoles asserts the security document carries the whole
// verified set, and that the set is still exactly that size.
func TestSecurityDocListsAllRoles(t *testing.T) {
	root := repoRoot(t)
	doc := readDoc(t, root, filepath.Join("docs", "security.md"))

	if len(roles) != 15 {
		t.Fatalf("the least-privilege set is defined as 15 roles, test lists %d", len(roles))
	}
	for _, r := range roles {
		if !strings.Contains(doc, r) {
			t.Errorf("role %s is missing from docs/security.md", r)
		}
	}

	// The shared-target exposure is an accepted risk; it must be stated where an
	// operator will read it before trusting ReadWriteOnce.
	for _, phrase := range []string{"shared", "ReadWriteOnce"} {
		if !strings.Contains(doc, phrase) {
			t.Errorf("docs/security.md does not mention %q, so the shared-target risk is not stated", phrase)
		}
	}
}

// TestReadmeWarnsAboutPlaintext asserts the README states the wss:// requirement
// and why it matters. A plaintext connection does not merely fail — it destroys
// the API key — so an operator must meet this warning before their first install.
func TestReadmeWarnsAboutPlaintext(t *testing.T) {
	root := repoRoot(t)
	readme := readDoc(t, root, "README.md")

	if !strings.Contains(readme, "wss://") {
		t.Error("README.md does not state that the endpoint must be wss://")
	}
	if !strings.Contains(strings.ToLower(readme), "revoke") {
		t.Error("README.md does not explain that TrueNAS revokes an API key presented over plaintext")
	}
}

// findAllParamKeys returns every StorageClass parameter name a source file
// reads. The backends use three different styles — a constant, a helper call,
// and direct map indexing — so matching only one of them would let an
// undocumented parameter slip past this test entirely.
func findAllParamKeys(src string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, re := range paramKeyREs {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			if _, ok := seen[m[1]]; ok {
				continue
			}
			seen[m[1]] = struct{}{}
			out = append(out, m[1])
		}
	}
	return out
}

// TestEveryParameterIsAccepted fails when the code reads a StorageClass
// parameter that CreateVolume would refuse.
//
// The driver now rejects a class carrying a key nothing reads, which is what
// turns a typo into a failure instead of a silent default. That refusal is only
// safe while the accepted set really is every key the backends read: a
// parameter added to a backend without being declared would make every class
// using it fail to provision.
//
// It reuses the same discovery the documentation check uses, so a new parameter
// is caught by whichever of the two is wrong.
func TestEveryParameterIsAccepted(t *testing.T) {
	root := repoRoot(t)

	accepted := map[string]bool{}
	for _, k := range csi.CommonParameters() {
		accepted[k] = true
	}
	for _, b := range []backend.Backend{
		nfs.New(nil, backend.Options{}),
		iscsi.New(nil, backend.Options{}),
		nvme.New(nil, backend.Options{}),
		smb.New(nil, backend.Options{}),
	} {
		for _, k := range b.AcceptedParameters() {
			accepted[k] = true
		}
	}
	for _, k := range node.NodeParameterKeys {
		accepted[k] = true
	}

	// "key" and "value" are the field names of the user_properties payload, not
	// StorageClass parameters; the discovery regexes cannot tell them apart.
	notParameters := map[string]bool{"key": true, "value": true, "password": true}

	for p, where := range paramsFromSources(t, root) {
		if notParameters[p] || strings.HasPrefix(p, "csi.storage.k8s.io/") {
			continue
		}
		if !accepted[p] {
			t.Errorf("%s reads the StorageClass parameter %q, but CreateVolume "+
				"rejects it: add it to the backend's AcceptedParameters", where, p)
		}
	}
}

// TestChartIndexIsBuiltFromReleases guards the Helm repository the
// documentation tells people to add.
//
// The docs workflow used to `helm package` the CHECKED-OUT chart into the site.
// Two things followed, both observed on the live site. The published chart
// carried whatever version Chart.yaml said, which between releases is the
// version already released — so `helm install --version 0.1.3` from the
// documented repository installed main's in-development chart, and the served
// 0.1.3 had a different digest from the v0.1.3 release asset. And because a
// Pages artifact replaces the whole site, re-indexing one freshly packaged
// chart erased every earlier version: the index listed exactly one.
//
// The index must therefore be built from the release artifacts, which are the
// only things that cannot drift from their tags.
func TestChartIndexIsBuiltFromReleases(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "docs.yaml"))
	if err != nil {
		t.Fatalf("read the docs workflow: %v", err)
	}
	wf := string(b)

	if strings.Contains(wf, "helm package deploy/helm/truenas-csi") {
		t.Error("the docs workflow packages the checked-out chart into the site, " +
			"which publishes unreleased content under a released version number")
	}
	if !strings.Contains(wf, "gh release download") {
		t.Error("the docs workflow does not take the charts from the releases, so " +
			"the published index can drift from the tags")
	}
	if !strings.Contains(wf, "helm repo index site/charts") {
		t.Error("the docs workflow no longer builds a Helm repository index")
	}
}
