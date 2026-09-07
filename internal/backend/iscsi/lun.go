package iscsi

import (
	"context"
	"fmt"
	"sync"

	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// maxLUNs is the ceiling a single target addresses. The shared-target model
// puts every volume on this backend on one target, so this is also the number
// of iSCSI volumes one backend can hold, and the driver must say so plainly
// rather than fail somewhere inside the middleware.
const maxLUNs = 255

var (
	// lunMu guards the two maps below, not the allocation itself.
	lunMu sync.Mutex
	// lunLocks serialises allocation per target: two volumes racing on the
	// same target must not read the same free id.
	lunLocks = map[int]*sync.Mutex{}
	// lunReserved holds ids handed out but not yet visible in a live query.
	//
	// This is NOT a cache of the allocation state — the live query remains the
	// source of truth and this map is empty after a restart. It exists only to
	// close the window between allocateLUN returning an id and the caller
	// creating the targetextent that makes the id observable.
	lunReserved = map[int]map[int]bool{}
)

// resetState discards every scrap of in-process allocation state, which is
// what a controller restart does. Nothing about correctness may depend on it
// surviving.
func resetState() {
	lunMu.Lock()
	defer lunMu.Unlock()
	lunLocks = map[int]*sync.Mutex{}
	lunReserved = map[int]map[int]bool{}
	resetTargetLocks()
}

func lunLock(targetID int) *sync.Mutex {
	lunMu.Lock()
	defer lunMu.Unlock()
	l, ok := lunLocks[targetID]
	if !ok {
		l = &sync.Mutex{}
		lunLocks[targetID] = l
	}
	return l
}

func reservations(targetID int) map[int]bool {
	lunMu.Lock()
	defer lunMu.Unlock()
	r, ok := lunReserved[targetID]
	if !ok {
		r = map[int]bool{}
		lunReserved[targetID] = r
	}
	return r
}

// allocateLUN returns the lowest free LUN id on a target.
//
// The used set is derived from a LIVE iscsi.targetextent.query on every call.
// Caching it would be faster and wrong: a controller restart would begin again
// at 0 and map a new volume onto a LUN another volume already occupies, which
// the initiator sees as the old device's contents changing underneath it.
func allocateLUN(ctx context.Context, c truenas.API, targetID int) (int, error) {
	l := lunLock(targetID)
	l.Lock()
	defer l.Unlock()

	mappings, err := c.TargetExtentList(ctx, targetID)
	if err != nil {
		return 0, fmt.Errorf("listing LUN mappings for target %d: %w", targetID, err)
	}

	used := make(map[int]bool, len(mappings))
	for _, m := range mappings {
		used[m.LUNID] = true
	}

	lunMu.Lock()
	res := lunReserved[targetID]
	if res == nil {
		res = map[int]bool{}
		lunReserved[targetID] = res
	}
	for id := range res {
		if used[id] {
			// The appliance now reports it: the reservation has served its
			// purpose and the live query carries the id from here on.
			delete(res, id)
			continue
		}
		used[id] = true
	}
	lunMu.Unlock()

	for i := 0; i < maxLUNs; i++ {
		if used[i] {
			continue
		}
		lunMu.Lock()
		lunReserved[targetID][i] = true
		lunMu.Unlock()
		return i, nil
	}
	return 0, fmt.Errorf("target %d holds the maximum of %d LUNs", targetID, maxLUNs)
}

// releaseLUN drops a reservation whose targetextent was never created, so a
// failed provision does not burn an id until the next restart.
func releaseLUN(targetID, lun int) {
	lunMu.Lock()
	defer lunMu.Unlock()
	if r, ok := lunReserved[targetID]; ok {
		delete(r, lun)
	}
}
