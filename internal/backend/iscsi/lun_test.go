package iscsi

import (
	"context"
	"sync"
	"testing"
)

// TestLUNAllocationUnderConcurrency is the test the shared-target design lives
// or dies by. Every volume on this backend is a LUN on ONE target, so a
// duplicated id maps two volumes onto the same LUN — silent data corruption —
// and an id that restarts at 0 after a controller restart does exactly that.
//
// It therefore checks two distinct properties:
//  1. twenty concurrent allocations produce twenty distinct ids, so the
//     allocation is serialised and reservations are visible across goroutines;
//  2. after ALL in-memory state is discarded, the next id continues from the
//     appliance's live mappings rather than from zero. That second half fails
//     the moment allocation is served from a cached counter.
func TestLUNAllocationUnderConcurrency(t *testing.T) {
	n := newNAS(t)
	c := n.client()
	ctx := context.Background()

	const targetID = 7
	const n1 = 20

	var (
		mu   sync.Mutex
		seen = map[int]bool{}
		wg   sync.WaitGroup
	)
	errs := make(chan error, n1)
	for i := 0; i < n1; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lun, err := allocateLUN(ctx, c, targetID)
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			if seen[lun] {
				mu.Unlock()
				errs <- errDuplicateLUN(lun)
				return
			}
			seen[lun] = true
			mu.Unlock()
			// Publishing the mapping is what makes the id visible to the live
			// query; the fake refuses a duplicate lunid on the same target.
			if _, err := c.TargetExtentCreate(ctx, targetID, 1000+i, lun); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent allocation: %v", err)
	}

	if len(seen) != n1 {
		t.Fatalf("want %d distinct LUN ids, got %d", n1, len(seen))
	}
	for i := 0; i < n1; i++ {
		if !seen[i] {
			t.Fatalf("LUN ids must be contiguous from 0; %d is missing (got %v)", i, seen)
		}
	}

	// Controller restart: every byte of in-process state is gone, but the
	// appliance still holds LUNs 0..19.
	resetState()

	lun, err := allocateLUN(ctx, c, targetID)
	if err != nil {
		t.Fatalf("allocateLUN after restart: %v", err)
	}
	if lun != n1 {
		t.Fatalf("after a controller restart allocation must continue from the live query: want %d, got %d", n1, lun)
	}
}

type errDuplicateLUN int

func (e errDuplicateLUN) Error() string {
	return "duplicate LUN id allocated: two volumes would share one LUN"
}

// TestLUNAllocationFillsHoles proves a deleted volume's id is reused rather
// than leaked, which is what keeps a long-lived backend under the target's LUN
// ceiling.
func TestLUNAllocationFillsHoles(t *testing.T) {
	n := newNAS(t)
	c := n.client()
	ctx := context.Background()

	const targetID = 3
	for _, lun := range []int{0, 1, 3} {
		if _, err := c.TargetExtentCreate(ctx, targetID, 100+lun, lun); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got, err := allocateLUN(ctx, c, targetID)
	if err != nil {
		t.Fatalf("allocateLUN: %v", err)
	}
	if got != 2 {
		t.Fatalf("want the free id 2, got %d", got)
	}
}

// TestLUNAllocationIsPerTarget guards against a single global counter: two
// backends sharing a process must each start their own target at 0.
func TestLUNAllocationIsPerTarget(t *testing.T) {
	n := newNAS(t)
	c := n.client()
	ctx := context.Background()

	if _, err := c.TargetExtentCreate(ctx, 1, 10, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := allocateLUN(ctx, c, 2)
	if err != nil {
		t.Fatalf("allocateLUN: %v", err)
	}
	if got != 0 {
		t.Fatalf("a second target starts at LUN 0, got %d", got)
	}
}
