// Package skew decides whether a driver version may be deployed alongside the
// CSI sidecar versions the chart pins.
//
// The check exists because a half-applied release is worse than an unchanged
// one. If a driver image is rolled out against sidecars it cannot speak to, the
// controller Deployment comes up, the external-provisioner starts failing calls
// it does not understand, and PVCs stop binding — while the DaemonSet is still
// mid-rollout and half the nodes cannot mount anything. Refusing the upgrade
// outright, with a reason on the resource, leaves a cluster that still works.
package skew

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ErrIncompatible is returned for any combination the operator refuses.
var ErrIncompatible = errors.New("version skew")

// SidecarFloor is the oldest release of each CSI sidecar this driver is known
// to work with. Raising a floor is a deliberate act: it makes the operator
// refuse every chart still pinning the older sidecar, which is exactly the
// point when the driver starts relying on a newer sidecar's behaviour.
var SidecarFloor = map[string]string{
	"provisioner":   "v5.1.0",
	"attacher":      "v4.7.0",
	"resizer":       "v1.12.0",
	"snapshotter":   "v8.1.0",
	"livenessprobe": "v2.14.0",
	"registrar":     "v2.12.0",
}

// SupportedDriverRange is the driver versions this operator knows how to
// manage. An operator asked to install a driver from the future is refusing to
// guess: it has no idea what that driver expects of its sidecars, of the chart
// values, or of the CRD it is being configured through.
const SupportedDriverRange = ">= 0.1.0-0, < 1.0.0-0"

// Check reports whether driverVersion may be deployed with the given sidecar
// images. sidecars maps the chart's sidecar key ("provisioner", …) to the full
// image reference the chart pins.
//
// An unparseable driver version is accepted: a tag like "main" or a digest-
// pinned development build is a deliberate act by whoever set it, and refusing
// it would make the operator unusable for the people developing the driver. An
// unparseable *sidecar* version is refused, because nobody sets that by hand —
// it comes from the chart, and a chart whose sidecar pin cannot be read is a
// chart the operator cannot reason about.
func Check(driverVersion string, sidecars map[string]string) error {
	if v, err := semver.NewVersion(strings.TrimPrefix(driverVersion, "v")); err == nil {
		constraint, cerr := semver.NewConstraint(SupportedDriverRange)
		if cerr != nil {
			return fmt.Errorf("%w: unusable supported range %q: %v", ErrIncompatible, SupportedDriverRange, cerr)
		}
		if !constraint.Check(v) {
			return fmt.Errorf("%w: driver version %s is outside the range this operator supports (%s); upgrade the operator first",
				ErrIncompatible, driverVersion, SupportedDriverRange)
		}
	}

	var problems []string
	for _, name := range sortedKeys(SidecarFloor) {
		ref, ok := sidecars[name]
		if !ok {
			// A chart that does not deploy a sidecar cannot skew against it.
			continue
		}
		got, err := versionOfImage(ref)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s image %q: %v", name, ref, err))
			continue
		}
		floor, err := semver.NewVersion(strings.TrimPrefix(SidecarFloor[name], "v"))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s floor %q is unparseable", name, SidecarFloor[name]))
			continue
		}
		if got.LessThan(floor) {
			problems = append(problems, fmt.Sprintf("%s %s is older than the required %s",
				name, got.Original(), SidecarFloor[name]))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: driver %s cannot run with these sidecars: %s",
			ErrIncompatible, driverVersion, strings.Join(problems, "; "))
	}
	return nil
}

// versionOfImage pulls the semantic version out of an image reference such as
// registry.k8s.io/sig-storage/csi-provisioner:v5.1.0.
func versionOfImage(ref string) (*semver.Version, error) {
	tag := ref
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		tag = ref[:i]
	}
	i := strings.LastIndex(tag, ":")
	if i < 0 || strings.Contains(tag[i+1:], "/") {
		return nil, errors.New("no tag to read a version from")
	}
	tag = tag[i+1:]
	v, err := semver.NewVersion(strings.TrimPrefix(tag, "v"))
	if err != nil {
		return nil, fmt.Errorf("tag %q is not a version: %w", tag, err)
	}
	return v, nil
}

// SidecarsFromValues reads the chart's `sidecars` map into image references.
func SidecarsFromValues(values map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := values["sidecars"].(map[string]any)
	if !ok {
		return out
	}
	for name, v := range raw {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if img, ok := entry["image"].(string); ok && img != "" {
			out[name] = img
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
