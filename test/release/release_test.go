// Package release_test verifies a published release the way a consumer would:
// against the registry, from outside the workflow that produced it. It asserts
// the tag resolves to a manifest list covering both architectures, that the
// image carries a valid keyless cosign signature from this repository's release
// workflow, and that an SBOM attestation is attached to it.
//
// It does nothing unless RELEASE_TAG names a tag to check, so `go test ./...`
// on a development machine costs nothing and needs no registry access.
package release_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// defaultImage is the repository the release workflow publishes to.
const defaultImage = "ghcr.io/piwi3910/truenas-csi"

// wantPlatforms is the release contract. arm64 is the primary architecture and
// the validation target; amd64 is built but not validated on hardware.
var wantPlatforms = []string{"linux/arm64", "linux/amd64"}

// commandTimeout bounds every registry round trip, so a hung pull fails the
// test rather than the job's wall clock.
const commandTimeout = 2 * time.Minute

type release struct {
	image  string // repository, without a tag
	tag    string
	digest string // manifest-list digest, when the caller knows it
}

// underTest returns the release to verify, skipping when none is named.
func underTest(t *testing.T) release {
	t.Helper()
	tag := os.Getenv("RELEASE_TAG")
	if tag == "" {
		t.Skip("RELEASE_TAG is not set: set it to a published tag (for example v0.1.0) to verify a release")
	}
	image := os.Getenv("RELEASE_IMAGE")
	if image == "" {
		image = defaultImage
	}
	return release{image: image, tag: tag, digest: os.Getenv("RELEASE_DIGEST")}
}

// ref is the reference to verify: the digest when it is known, because a tag
// can be moved after it was signed and a digest cannot.
func (r release) ref() string {
	if r.digest != "" {
		return r.image + "@" + r.digest
	}
	return r.image + ":" + r.tag
}

// tool returns an installed binary, skipping the test when it is absent.
func tool(t *testing.T, name, why string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed: %s", name, why)
	}
	return path
}

// run executes a command and returns its combined output, failing the test with
// that output when it exits non-zero.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// manifestList is the subset of an OCI image index this test cares about.
type manifestList struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// TestReleaseArtifacts is the release gate: a tag that fails any part of this
// is not a release anyone should pull.
func TestReleaseArtifacts(t *testing.T) {
	rel := underTest(t)

	t.Run("manifest lists both architectures", func(t *testing.T) {
		docker := tool(t, "docker", "needed to inspect the published manifest list")

		var index manifestList
		raw := run(t, docker, "manifest", "inspect", rel.ref())
		if err := json.Unmarshal([]byte(raw), &index); err != nil {
			t.Fatalf("parse manifest of %s: %v\n%s", rel.ref(), err, raw)
		}
		if len(index.Manifests) == 0 {
			t.Fatalf("%s is a single-architecture image, not a manifest list:\n%s", rel.ref(), raw)
		}

		got := map[string]bool{}
		for _, m := range index.Manifests {
			// Attestation manifests ride along in the index with an
			// architecture of "unknown"; they are not platforms.
			if m.Platform.Architecture == "unknown" || m.Platform.OS == "unknown" {
				continue
			}
			got[m.Platform.OS+"/"+m.Platform.Architecture] = true
		}
		for _, want := range wantPlatforms {
			if !got[want] {
				t.Errorf("manifest list for %s does not include %s (has %v)", rel.ref(), want, keys(got))
			}
		}
	})

	t.Run("cosign signature", func(t *testing.T) {
		cosign := tool(t, "cosign", "needed to verify the keyless signature")

		identity := os.Getenv("COSIGN_CERTIFICATE_IDENTITY_REGEXP")
		issuer := os.Getenv("COSIGN_CERTIFICATE_OIDC_ISSUER")
		if identity == "" || issuer == "" {
			// Verifying without pinning the identity would accept a signature
			// from anyone at all, which is worse than not checking.
			t.Skip("COSIGN_CERTIFICATE_IDENTITY_REGEXP and COSIGN_CERTIFICATE_OIDC_ISSUER must both be set: " +
				"an unpinned cosign verify accepts any signer")
		}

		run(t, cosign, "verify",
			"--certificate-identity-regexp", identity,
			"--certificate-oidc-issuer", issuer,
			rel.ref())
	})

	t.Run("sbom attestation", func(t *testing.T) {
		cosign := tool(t, "cosign", "needed to verify the SBOM attestation")

		identity := os.Getenv("COSIGN_CERTIFICATE_IDENTITY_REGEXP")
		issuer := os.Getenv("COSIGN_CERTIFICATE_OIDC_ISSUER")
		if identity == "" || issuer == "" {
			t.Skip("COSIGN_CERTIFICATE_IDENTITY_REGEXP and COSIGN_CERTIFICATE_OIDC_ISSUER must both be set")
		}

		out := run(t, cosign, "verify-attestation",
			"--type", "spdxjson",
			"--certificate-identity-regexp", identity,
			"--certificate-oidc-issuer", issuer,
			rel.ref())

		// cosign writes its human-readable verification block to stderr and a
		// DSSE envelope to stdout, and the envelope's payload is BASE64 — so
		// grepping the combined output for "spdx" tests nothing: it cannot match
		// a valid SBOM, and would match a stray mention in the preamble. The
		// payload has to be decoded and inspected.
		env, err := dssePayload(out)
		if err != nil {
			t.Fatalf("reading the attestation envelope for %s: %v\n%s", rel.ref(), err, out)
		}
		var doc struct {
			Predicate struct {
				SPDXVersion string `json:"spdxVersion"`
				SPDXID      string `json:"SPDXID"`
				Packages    []struct {
					Name string `json:"name"`
				} `json:"packages"`
			} `json:"predicate"`
		}
		if err := json.Unmarshal(env, &doc); err != nil {
			t.Fatalf("decoding the attestation predicate for %s: %v", rel.ref(), err)
		}
		if doc.Predicate.SPDXVersion == "" {
			t.Errorf("the attestation on %s carries no spdxVersion, so it is not an SPDX SBOM:\n%s",
				rel.ref(), truncate(string(env), 2000))
		}
		// An SBOM listing nothing is not an SBOM. syft on a scratch image with a
		// single static binary still reports the binary and its Go modules.
		if len(doc.Predicate.Packages) == 0 {
			t.Errorf("the SBOM attested on %s lists no packages", rel.ref())
		}
		t.Logf("SBOM %s lists %d packages", doc.Predicate.SPDXVersion, len(doc.Predicate.Packages))
	})
}

// TestReleaseImageRunsTheDriver checks the published image actually contains a
// working driver binary reporting the expected identity — a manifest list of
// broken images would pass every signature check above.
func TestReleaseImageRunsTheDriver(t *testing.T) {
	rel := underTest(t)
	docker := tool(t, "docker", "needed to run the published image")

	out := run(t, docker, "run", "--rm", "--pull=always", rel.ref(), "-version")
	if !strings.Contains(out, "csi.truenas.watteel.com") {
		t.Errorf("image %s does not report the driver name; got:\n%s", rel.ref(), out)
	}
	if !strings.Contains(out, strings.TrimPrefix(rel.tag, "v")) {
		t.Errorf("image %s does not report version %s; got:\n%s", rel.ref(), rel.tag, out)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// dssePayload extracts and decodes the base64 payload of the first DSSE
// envelope in cosign's output, ignoring the human-readable lines around it.
func dssePayload(out string) ([]byte, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var env struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil || env.Payload == "" {
			continue
		}
		return base64.StdEncoding.DecodeString(env.Payload)
	}
	return nil, errors.New("no DSSE envelope with a payload found in the output")
}

// truncate keeps a failure message readable when the payload is enormous.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
