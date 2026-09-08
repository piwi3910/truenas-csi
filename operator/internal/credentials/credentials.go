// Package credentials resolves the Secrets a TrueNASCSIDriver references and
// decides what to restart when one of them changes.
//
// A CSI driver reads its API key once, at startup, and keeps the websocket
// session open for the life of the process. Rotating the key on the appliance
// and updating the Secret therefore changes nothing until the pods restart —
// they keep presenting the old key until the appliance stops accepting it, at
// which point provisioning breaks with an authentication error that looks like
// a driver bug. Worse, the driver treats an authentication failure as terminal
// and never retries it, so the pods do not recover on their own.
//
// So the operator watches the referenced Secrets, fingerprints them, and
// restarts the driver in a controlled order when the fingerprint moves.
package credentials

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Ref is one resolved Secret key.
type Ref struct {
	Secret          string
	Key             string
	ResourceVersion string
}

// Fingerprint turns the observed Secret resourceVersions into a short, stable
// string. It is derived from resourceVersions, never from the key material: the
// fingerprint ends up in the resource's status, which is world-readable to
// anyone who can read the CR, and a hash of a secret is still a secret oracle.
func Fingerprint(refs []Ref) string {
	ordered := make([]Ref, len(refs))
	copy(ordered, refs)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Secret != ordered[j].Secret {
			return ordered[i].Secret < ordered[j].Secret
		}
		return ordered[i].Key < ordered[j].Key
	})
	h := sha256.New()
	for _, r := range ordered {
		// hash.Hash.Write is documented never to return an error, so the
		// discard is the honest form rather than a check that can never fire.
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00", r.Secret, r.Key, r.ResourceVersion)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Stage is one step of a credential rotation.
type Stage string

const (
	// StageNone means nothing needs restarting.
	StageNone Stage = "None"
	// StageController restarts the controller pods first. The controller is the
	// only component that provisions, so it is the one whose stale key does
	// visible damage first, and it is safe to restart at any moment: leader
	// election hands provisioning to the standby.
	StageController Stage = "Controller"
	// StageNodes rolls the node plugin pods, through the drain-aware rollout, so
	// no node loses its plugin while a volume is mid-stage.
	StageNodes Stage = "Nodes"
	// StageWaitController means the controller restart is still in flight. The
	// node roll does not start until the controller is healthy on the new key:
	// if the new key is wrong, the failure shows up on two controller pods
	// rather than on every node in the cluster at once.
	StageWaitController Stage = "WaitingForController"
)

// Rotation describes what to do about a credential change.
type Rotation struct {
	Stage   Stage
	Reason  string
	Changed bool
}

// Plan decides the next rotation step.
//
// observed is the fingerprint recorded in status, desired the one just computed.
// controllerAtDesired reports whether the controller pods are already running
// with the desired credentials, and controllerReady whether they are healthy.
func Plan(observed, desired string, controllerAtDesired, controllerReady bool) Rotation {
	if observed == desired {
		return Rotation{Stage: StageNone}
	}
	if observed == "" {
		// Nothing was ever recorded, so this is a first install rather than a
		// rotation. There are no pods holding a stale key, and staging the
		// install through a controller restart would only make it slower.
		return Rotation{Stage: StageNodes, Reason: "initial install"}
	}
	if !controllerAtDesired {
		return Rotation{
			Stage:   StageController,
			Changed: true,
			Reason:  "credentials changed: restarting the controller before touching any node",
		}
	}
	if !controllerReady {
		return Rotation{
			Stage:   StageWaitController,
			Changed: true,
			Reason:  "waiting for the controller to become healthy on the new credentials before rolling nodes",
		}
	}
	return Rotation{
		Stage:   StageNodes,
		Changed: true,
		Reason:  "controller healthy on the new credentials: rolling node plugins",
	}
}

// Validate rejects resolved key material that cannot possibly work, before it
// is rendered into a Secret and mounted.
func Validate(backend, key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("backend %q: API key is empty", backend)
	}
	if strings.ContainsAny(key, "\n\r") {
		return fmt.Errorf("backend %q: API key contains a newline, which usually means the Secret was created from a file with a trailing newline", backend)
	}
	return nil
}
