// Package backend defines the protocol-independent provisioning interface and
// the registry of TrueNAS appliances the driver provisions against.
package backend

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/piwi3910/truenas-csi/internal/retention"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Volume is a provisioned volume as the controller reports it.
type Volume struct {
	ID            volume.ID
	CapacityBytes int64
	Context       map[string]string
}

// CreateRequest is a provisioning request.
type CreateRequest struct {
	ID             volume.ID
	CapacityBytes  int64
	Params         map[string]string
	SourceSnapshot string // when set, clone from this snapshot instead of creating empty
}

// Backend provisions volumes of one protocol on one appliance.
//
// A third protocol is an added implementation, not a change here: SMB and
// NVMe-oF were both validated against the live appliance and fit this shape.
type Backend interface {
	// Protocol is the StorageClass "protocol" value this backend serves.
	Protocol() string
	// Create provisions a volume, or returns the existing one when an identical
	// volume already exists. It must be safe to call repeatedly.
	Create(ctx context.Context, r CreateRequest) (*Volume, error)
	// Delete removes a volume. Deleting an absent volume is success; deleting a
	// dataset this driver does not own must fail rather than destroy data.
	Delete(ctx context.Context, id volume.ID) error
	// Expand grows a volume and returns the new size. Shrink must be refused.
	Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error)
	// PublishContext is the map handed to the node plugin to attach the volume.
	PublishContext(ctx context.Context, id volume.ID) (map[string]string, error)
	// AcceptedParameters lists the StorageClass parameters this backend reads,
	// so CreateVolume can refuse the ones it does not.
	//
	// A StorageClass is immutable, and an unknown key was silently ignored: a
	// class saying nfsVersionn: "3" provisioned NFSv4 and said nothing. The
	// same typo in maproot, mode or networks silently drops a security setting
	// the operator believes is applied.
	AcceptedParameters() []string
	// MinimumCapacityBytes is the smallest volume this backend can actually
	// create, 0 when it has no floor.
	//
	// It exists because TrueNAS refuses a refquota below 1 GiB outright, so
	// every filesystem-backed claim smaller than that failed to provision --
	// with a Pydantic union error naming neither the limit nor the field the
	// caller set. CreateVolume rounds up to this and reports the rounded size,
	// which CSI allows, and refuses only when the claim's limit_bytes puts the
	// floor out of reach.
	MinimumCapacityBytes() int64
}

// Options is everything a backend needs about the appliance it serves beyond
// the connection itself.
//
// It is a struct rather than three positional strings because the third thing a
// backend needs is a POLICY, not a name: whether DeleteVolume destroys a dataset
// or retires it. A reader of `New(c, "Pool0", "k8s", 168*time.Hour, ".trash")`
// would have had no chance.
type Options struct {
	// Pool and Parent are the operator-configured location the backend is
	// confined to.
	Pool   string
	Parent string

	// Retention is the delete-protection policy. Its zero value is "off", which
	// is the default and takes exactly the code path the driver has always
	// taken.
	Retention retention.Policy
}

// Factory builds a Backend for one appliance.
type Factory func(c truenas.API, opts Options) Backend

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register makes a protocol available to the registry. Protocol packages call
// this from init(); the registry lives here, so registration rather than a
// direct import is what keeps this package free of a cycle.
func Register(protocol string, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[protocol] = f
}

func factoryFor(protocol string) (Factory, bool) {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	f, ok := factories[protocol]
	return f, ok
}

// Protocols lists the registered protocol names.
func Protocols() []string {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownBackend means a StorageClass named an appliance that is not configured.
var ErrUnknownBackend = fmt.Errorf("storage class names a backend that is not configured")

// ErrUnsupportedProtocol means the protocol is not built into this version.
var ErrUnsupportedProtocol = fmt.Errorf("protocol is not supported by this driver version")
