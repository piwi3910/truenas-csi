// Package backend defines the protocol-independent provisioning interface and
// the registry of TrueNAS appliances the driver provisions against.
package backend

import (
	"context"
	"fmt"
	"sort"
	"sync"

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
}

// Factory builds a Backend for one appliance.
type Factory func(c *truenas.Client, pool, parent string) Backend

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
