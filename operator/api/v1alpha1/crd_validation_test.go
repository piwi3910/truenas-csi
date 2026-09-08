package v1alpha1_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdPath is the generated CRD the operator ships. The tests below read the
// generated file rather than the Go markers, because the generated file is what
// the API server actually enforces: a marker that fails to survive code
// generation is a validation that does not exist in a cluster.
const crdPath = "../../config/crd/bases/truenas.watteel.com_truenascsidrivers.yaml"

func loadCRD(t *testing.T) *apiextv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(crdPath))
	if err != nil {
		t.Fatalf("read generated CRD (run `make manifests`): %v", err)
	}
	crd := &apiextv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatalf("parse generated CRD: %v", err)
	}
	return crd
}

func specSchema(t *testing.T, crd *apiextv1.CustomResourceDefinition) apiextv1.JSONSchemaProps {
	t.Helper()
	for _, v := range crd.Spec.Versions {
		if v.Name != "v1alpha1" || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			t.Fatal("v1alpha1 schema has no spec")
		}
		return spec
	}
	t.Fatal("CRD has no v1alpha1 version with a schema")
	return apiextv1.JSONSchemaProps{}
}

func backendSchema(t *testing.T) apiextv1.JSONSchemaProps {
	t.Helper()
	spec := specSchema(t, loadCRD(t))
	backends, ok := spec.Properties["backends"]
	if !ok {
		t.Fatal("spec has no backends property")
	}
	if backends.Items == nil || backends.Items.Schema == nil {
		t.Fatal("spec.backends has no item schema")
	}
	return *backends.Items.Schema
}

// TestCRDRejectsPlaintextEndpoint is the most important test in this package.
//
// TrueNAS 25.10 REVOKES an API key the moment it is presented over a plaintext
// connection. A `ws://` endpoint that reaches a running driver does not fail
// safely — it destroys the credential, and someone has to issue a new one by
// hand. The endpoint pattern in the CRD is what stops such a value ever being
// persisted, so the pattern itself is worth testing directly.
//
// The pattern is evaluated exactly as the API server evaluates it: Kubernetes
// compiles `pattern` with Go's regexp and calls MatchString, which is an
// UNANCHORED match. An expression without ^ and $ would accept
// "http://evil/?redirect=wss://nas", which is why the anchors are checked here
// by consequence rather than by inspection.
func TestCRDRejectsPlaintextEndpoint(t *testing.T) {
	endpoint, ok := backendSchema(t).Properties["endpoint"]
	if !ok {
		t.Fatal("backend schema has no endpoint property")
	}
	if endpoint.Pattern == "" {
		t.Fatal("backend endpoint has no pattern: a plaintext endpoint would be accepted and would revoke the API key")
	}
	re, err := regexp.Compile(endpoint.Pattern)
	if err != nil {
		t.Fatalf("endpoint pattern %q does not compile the way the API server compiles it: %v", endpoint.Pattern, err)
	}

	rejected := []string{
		"ws://nas1.example.com/api/current",
		"http://nas1.example.com/api/current",
		"https://nas1.example.com/api/current",
		"tcp://nas1.example.com",
		"nas1.example.com",
		"WSS://nas1.example.com/api/current", // scheme comparison is case sensitive here on purpose
		"wss://",
		"",
		" wss://nas1.example.com/api/current",
		"http://evil.example.com/?redirect=wss://nas1.example.com",
		"ws://nas1.example.com/api/current\nwss://nas1.example.com/api/current",
	}
	for _, e := range rejected {
		if re.MatchString(e) {
			t.Errorf("endpoint %q is accepted by pattern %q; a non-wss endpoint reaching the driver revokes the API key",
				e, endpoint.Pattern)
		}
	}

	accepted := []string{
		"wss://nas1.example.com/api/current",
		"wss://nas1.example.com",
		"wss://10.0.0.5:443/api/current",
		"wss://nas1.example.com:8443/api/current",
	}
	for _, e := range accepted {
		if !re.MatchString(e) {
			t.Errorf("endpoint %q is rejected by pattern %q but is a legitimate wss endpoint", e, endpoint.Pattern)
		}
	}

	if endpoint.MinLength == nil || *endpoint.MinLength < 8 {
		t.Error("endpoint has no useful minLength; an empty string should never reach the driver")
	}
}

// TestCRDRejectsInlineAPIKey checks that there is nowhere in the whole schema to
// type a credential.
//
// A key typed into a CR would be readable by anyone with `get
// truenascsidrivers`, would land in every backup of the cluster's resource
// inventory, and would be pasted into bug reports along with the rest of
// `kubectl get -o yaml`. The CRD's structural schema prunes unknown fields, so a
// user who writes `apiKey:` gets it silently dropped rather than stored — but
// only for as long as no future field named like a credential is added. This
// test is what keeps that true.
func TestCRDRejectsInlineAPIKey(t *testing.T) {
	crd := loadCRD(t)

	forbidden := regexp.MustCompile(`(?i)^(api_?key|password|passwd|secret|token|credential)s?$`)
	var walk func(path string, s apiextv1.JSONSchemaProps)
	walk = func(path string, s apiextv1.JSONSchemaProps) {
		for name, sub := range s.Properties {
			// A *reference* to a secret is fine and is the whole point; a field
			// that holds the material itself is not.
			isRef := name == "apiKeySecretRef" || name == "caCertSecretRef"
			if forbidden.MatchString(name) && !isRef {
				t.Errorf("%s.%s looks like it holds credential material inline; credentials must only ever be referenced", path, name)
			}
			walk(path+"."+name, sub)
		}
		if s.Items != nil && s.Items.Schema != nil {
			walk(path+"[]", *s.Items.Schema)
		}
		if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
			walk(path+"{}", *s.AdditionalProperties.Schema)
		}
	}
	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		walk(v.Name, *v.Schema.OpenAPIV3Schema)
	}

	// The schema must be structural and pruning: without this, an `apiKey` typed
	// under spec.backends[] would be persisted verbatim as an unknown field.
	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		if v.Schema.OpenAPIV3Schema.XPreserveUnknownFields != nil && *v.Schema.OpenAPIV3Schema.XPreserveUnknownFields {
			t.Errorf("version %s preserves unknown fields, so a typed-in apiKey would be stored", v.Name)
		}
	}
	if crd.Spec.PreserveUnknownFields {
		t.Error("CRD preserves unknown fields, so a typed-in apiKey would be stored")
	}

	// And the reference itself must be present, so that "no inline key" does not
	// simply mean "no way to supply a key at all".
	backend := backendSchema(t)
	ref, ok := backend.Properties["apiKeySecretRef"]
	if !ok {
		t.Fatal("backend has no apiKeySecretRef: there would be no way to supply a credential")
	}
	if _, ok := ref.Properties["name"]; !ok {
		t.Error("apiKeySecretRef has no name")
	}
	required := map[string]bool{}
	for _, f := range backend.Required {
		required[f] = true
	}
	if !required["apiKeySecretRef"] {
		t.Error("apiKeySecretRef is optional; a backend with no credential reference would be accepted")
	}
}

// TestCRDRejectsBothReserveForms guards the CEL rule that stops a pool reserve
// being expressed two contradictory ways at once.
func TestCRDRejectsBothReserveForms(t *testing.T) {
	backend := backendSchema(t)
	found := false
	for _, rule := range backend.XValidations {
		if rule.Rule == "!(has(self.reservedBytes) && has(self.reservedPercent))" {
			found = true
		}
	}
	if !found {
		t.Error("backend has no rule forbidding reservedBytes and reservedPercent together")
	}
}

// imageSchema returns the generated schema for spec.image.
func imageSchema(t *testing.T) apiextv1.JSONSchemaProps {
	t.Helper()
	spec := specSchema(t, loadCRD(t))
	image, ok := spec.Properties["image"]
	if !ok {
		t.Fatal("spec has no image property")
	}
	return image
}

// selfMatches pulls the argument of every `self.matches("…")` call out of a CEL
// rule.
//
// The rule is evaluated below rather than merely inspected, because a rule that
// is present but wrong is indistinguishable from a rule that is absent. CEL's
// matches() is RE2 and, like the API server's `pattern` handling, performs an
// UNANCHORED search — which is Go's regexp.MatchString exactly, so the tags in
// the table are judged the way a cluster would judge them.
func selfMatches(t *testing.T, rule string) []*regexp.Regexp {
	t.Helper()
	call := regexp.MustCompile(`self\.matches\("((?:[^"\\]|\\.)*)"\)`)
	var out []*regexp.Regexp
	for _, m := range call.FindAllStringSubmatch(rule, -1) {
		re, err := regexp.Compile(m[1])
		if err != nil {
			t.Fatalf("regex %q in the tag rule does not compile the way CEL compiles it: %v", m[1], err)
		}
		out = append(out, re)
	}
	return out
}

// TestCRDRejectsMalformedDriverVersion guards the rule that makes a mistyped
// driver version fail at `kubectl apply` rather than several minutes later, as
// an image pull failure on a pod nobody is watching.
//
// Dell's equivalent CRD accepts any string in this field and fails late; a typo
// like "v0.5" costs the administrator a rollout to discover. The floating tags
// a developer uses on purpose ("main", "pr-412") must still be accepted,
// because the operator deliberately declines to gate a version it cannot parse.
func TestCRDRejectsMalformedDriverVersion(t *testing.T) {
	tag, ok := imageSchema(t).Properties["tag"]
	if !ok {
		t.Fatal("image schema has no tag property")
	}
	if tag.Pattern == "" {
		t.Fatal("image tag has no pattern")
	}
	pattern, err := regexp.Compile(tag.Pattern)
	if err != nil {
		t.Fatalf("tag pattern %q does not compile: %v", tag.Pattern, err)
	}
	if len(tag.XValidations) != 1 {
		t.Fatalf("image tag has %d CEL rules, want exactly the one asserting that a version-shaped tag is a version",
			len(tag.XValidations))
	}
	if tag.XValidations[0].Message == "" {
		t.Error("the tag rule has no message; a rejection an administrator cannot read is one they cannot act on")
	}
	res := selfMatches(t, tag.XValidations[0].Rule)
	if len(res) != 2 {
		t.Fatalf("the tag rule contains %d self.matches() calls, want the version-shaped test and the semver test", len(res))
	}
	looksLikeAVersion, isAVersion := res[0], res[1]

	// accepted mirrors `pattern && (!looksLikeAVersion || isAVersion)`, which
	// is the conjunction the API server applies.
	accepted := func(v string) bool {
		return pattern.MatchString(v) && (!looksLikeAVersion.MatchString(v) || isAVersion.MatchString(v))
	}

	tests := []struct {
		tag  string
		want bool
		why  string
	}{
		{tag: "0.1.0", want: true, why: "the shipped release form"},
		{tag: "v0.1.0", want: true, why: "the v-prefixed form of the same version"},
		{tag: "0.2.0-rc.1", want: true, why: "a release candidate"},
		{tag: "v1.10.12", want: true, why: "double-digit components"},
		{tag: "main", want: true, why: "a development build off the default branch"},
		{tag: "pr-412", want: true, why: "a per-pull-request image"},
		{tag: "latest", want: true, why: "not advisable, but a deliberate choice this rule is not about"},
		{tag: "v0.5", want: false, why: "the typo this rule exists for: version-shaped, missing a component"},
		{tag: "0.1", want: false, why: "the same typo without the v"},
		{tag: "0.1.0.2", want: false, why: "a fourth component is not a semantic version"},
		{tag: "v01.2.3", want: false, why: "leading zeroes are not a semantic version"},
		{tag: "1.2.3-", want: false, why: "an empty pre-release"},
		{tag: "v", want: true, why: "not version-shaped at all, so the rule does not apply"},
		{tag: "-0.1.0", want: false, why: "the pattern refuses a leading dash"},
		{tag: "0.1.0 ", want: false, why: "trailing whitespace is not a tag"},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			if got := accepted(tt.tag); got != tt.want {
				t.Errorf("tag %q accepted = %v, want %v (%s)", tt.tag, got, tt.want, tt.why)
			}
		})
	}
}
