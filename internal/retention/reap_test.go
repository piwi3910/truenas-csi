package retention

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// week is the grace period every test here uses.
const week = "168h"

var (
	// now is the reaper's clock in these tests.
	now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	// longAgo is comfortably past a week's grace; justNow is not.
	longAgo = volume.FormatDeletedAt(now.Add(-30 * 24 * time.Hour))
	justNow = volume.FormatDeletedAt(now.Add(-time.Hour))
)

// dataset builds the middleware's view of a dataset with LOCAL user properties,
// plus any inherited ones, so a table row can state exactly the shape it means.
//
// It goes through the client's own JSON decoder rather than assembling the
// struct, because the property source — the field every ownership rule turns on
// — is decoded from the wire shape and a test that set it by hand would be
// testing its own assumption about that shape.
func dataset(t *testing.T, id string, local, inherited map[string]string) *truenas.Dataset {
	t.Helper()
	props := map[string]any{}
	for k, v := range inherited {
		props[k] = map[string]any{"value": v, "source": "INHERITED"}
	}
	for k, v := range local {
		props[k] = map[string]any{"value": v, "source": "LOCAL"}
	}
	raw, err := json.Marshal(map[string]any{"id": id, "type": "FILESYSTEM", "user_properties": props})
	if err != nil {
		t.Fatalf("marshal dataset: %v", err)
	}
	var ds truenas.Dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatalf("decode dataset: %v", err)
	}
	return &ds
}

// retiredProps is what Retire leaves on a dataset.
func retiredProps(deletedAt string) map[string]string {
	return map[string]string{
		volume.OwnerProperty:       volume.OwnerValue,
		volume.RetiredFromProperty: "nas1/nfs/Pool0/k8s/pvc-1",
		volume.DeletedAtProperty:   deletedAt,
	}
}

// TestReapableEnforcesAllFourPreconditions is the safety table. Every row that
// expects a refusal describes a dataset the driver must never destroy, and each
// one fails a DIFFERENT precondition, so a guard that collapses two of them into
// one still fails here.
func TestReapableEnforcesAllFourPreconditions(t *testing.T) {
	p := testPolicy(week)

	cases := []struct {
		name string
		ds   *truenas.Dataset
		want string // "" means reapable
	}{
		{
			name: "a retired volume past its grace period is reapable",
			ds:   dataset(t, p.Root()+"/20260801T000000Z-pvc-1", retiredProps(longAgo), nil),
		},
		{
			name: "exactly at the grace period is reapable",
			ds: dataset(t, p.Root()+"/at-the-boundary",
				retiredProps(volume.FormatDeletedAt(now.Add(-7*24*time.Hour))), nil),
		},

		// 1. Inside the graveyard.
		{
			name: "a live volume beside the graveyard is refused",
			ds:   dataset(t, "Pool0/k8s/pvc-live", retiredProps(longAgo), nil),
			want: "not inside the graveyard",
		},
		{
			name: "a dataset in a lookalike sibling of the graveyard is refused",
			ds:   dataset(t, "Pool0/k8s"+"/"+p.Graveyard+"-old/pvc-1", retiredProps(longAgo), nil),
			want: "not inside the graveyard",
		},
		{
			name: "a dataset under a DIFFERENT parent is refused",
			ds:   dataset(t, "Pool0/other/"+p.Graveyard+"/pvc-1", retiredProps(longAgo), nil),
			want: "not inside the graveyard",
		},
		{
			name: "the graveyard itself is refused",
			ds: dataset(t, p.Root(), map[string]string{
				volume.OwnerProperty:     volume.OwnerValue,
				volume.GraveyardProperty: volume.GraveyardValue,
				volume.DeletedAtProperty: longAgo,
			}, nil),
			want: "not inside the graveyard",
		},
		{
			name: "a dataset nested below a graveyard entry is refused",
			ds:   dataset(t, p.Root()+"/entry/child", retiredProps(longAgo), nil),
			want: "levels below",
		},

		// 2. Driver-owned, with source LOCAL.
		{
			name: "an unmarked dataset in the graveyard is refused",
			ds: dataset(t, p.Root()+"/hand-made", map[string]string{
				volume.DeletedAtProperty: longAgo,
			}, nil),
			want: "not managed by this driver",
		},
		{
			name: "a dataset that only INHERITED the marker from the graveyard is refused",
			ds: dataset(t, p.Root()+"/dropped-in",
				map[string]string{volume.DeletedAtProperty: longAgo},
				map[string]string{volume.OwnerProperty: volume.OwnerValue}),
			want: "not managed by this driver",
		},

		// 3. Carries a deletion timestamp.
		{
			name: "a driver-owned dataset with no deletion timestamp is refused",
			ds: dataset(t, p.Root()+"/no-timestamp",
				map[string]string{volume.OwnerProperty: volume.OwnerValue}, nil),
			want: "no io.truenas.csi:deleted-at property",
		},
		{
			name: "a timestamp that only INHERITED down is refused",
			ds: dataset(t, p.Root()+"/inherited-timestamp",
				map[string]string{volume.OwnerProperty: volume.OwnerValue},
				map[string]string{volume.DeletedAtProperty: longAgo}),
			want: "no io.truenas.csi:deleted-at property",
		},
		{
			name: "an unparsable timestamp is refused rather than treated as old",
			ds:   dataset(t, p.Root()+"/bad-timestamp", retiredProps("last tuesday"), nil),
			want: "is not an RFC 3339 timestamp",
		},

		// 4. Past its grace period.
		{
			name: "a volume retired an hour ago is refused",
			ds:   dataset(t, p.Root()+"/fresh", retiredProps(justNow), nil),
			want: "the grace period is 168h0m0s",
		},
		{
			name: "a timestamp in the future is refused, not treated as expired",
			ds: dataset(t, p.Root()+"/from-the-future",
				retiredProps(volume.FormatDeletedAt(now.Add(24*time.Hour))), nil),
			want: "the grace period is 168h0m0s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Reapable(tc.ds, p, now)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want reapable, got refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a refusal mentioning %q, got nil — this dataset would be DESTROYED", tc.want)
			}
			if !errors.Is(err, ErrNotReapable) {
				t.Errorf("refusal must wrap ErrNotReapable, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestReapableRefusesEverythingWhenProtectionIsOff pins the disabled case: a
// zero policy has no graveyard, so nothing can be inside one.
func TestReapableRefusesEverythingWhenProtectionIsOff(t *testing.T) {
	off := Policy{Pool: "Pool0", Parent: "k8s", Graveyard: ".trash"}
	ds := dataset(t, "Pool0/k8s/.trash/pvc-1", retiredProps(longAgo), nil)
	if err := Reapable(ds, off, now); err == nil {
		t.Fatal("a policy with no grace period must never authorise a destroy")
	}
}

// TestReaperNeverDestroysOutsideTheGraveyard is the first of the two tests that
// keep this feature from becoming a data-loss feature.
//
// The appliance is made to answer the graveyard listing with a live volume and
// an operator's own dataset as well — the shape a wrong prefix, a broadened
// filter or a query bug would produce — and the reaper must destroy neither.
func TestReaperNeverDestroysOutsideTheGraveyard(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)

	n.put(p.Root(), map[string]string{
		volume.OwnerProperty: volume.OwnerValue, volume.GraveyardProperty: volume.GraveyardValue})
	n.putRetired(p.Root()+"/20260801T000000Z-pvc-dead", "nas1/nfs/Pool0/k8s/pvc-dead", longAgo)
	// A live volume, and an operator's dataset outside the driver's parent.
	// Both carry the shape that would make them reapable if the graveyard
	// check were missing: driver-owned and long past any grace period.
	n.putRetired("Pool0/k8s/pvc-live", "nas1/nfs/Pool0/k8s/pvc-live", longAgo)
	n.putRetired("Pool0/Home", "nas1/nfs/Pool0/k8s/pvc-whatever", longAgo)

	// The graveyard listing is answered with EVERY dataset on the box rather
	// than only what the prefix filter asked for. That is the failure this test
	// exists for: the reaper must be safe because of what it CHECKS, not
	// because the appliance happened to answer narrowly.
	n.listEverything()

	r := &Reaper{Now: func() time.Time { return now },
		Backends: func(context.Context) []Target {
			return []Target{{Name: "nas1", Client: n.client(t), Policy: p}}
		}}
	destroyed := r.RunOnce(context.Background())

	if len(destroyed) != 1 || destroyed[0] != p.Root()+"/20260801T000000Z-pvc-dead" {
		t.Fatalf("reaper destroyed %v, want only the graveyard entry", destroyed)
	}
	for _, keep := range []string{"Pool0/k8s/pvc-live", "Pool0/Home", p.Root()} {
		if !n.has(keep) {
			t.Errorf("reaper destroyed %s, which is outside the graveyard", keep)
		}
	}
}

// TestReaperNeverDestroysBeforeTheGracePeriodExpires is the second.
func TestReaperNeverDestroysBeforeTheGracePeriodExpires(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)

	n.put(p.Root(), map[string]string{
		volume.OwnerProperty: volume.OwnerValue, volume.GraveyardProperty: volume.GraveyardValue})
	fresh := p.Root() + "/20260908T110000Z-pvc-fresh"
	expired := p.Root() + "/20260801T000000Z-pvc-old"
	n.putRetired(fresh, "nas1/nfs/Pool0/k8s/pvc-fresh", justNow)
	n.putRetired(expired, "nas1/nfs/Pool0/k8s/pvc-old", longAgo)

	client := n.client(t)
	target := func(context.Context) []Target {
		return []Target{{Name: "nas1", Client: client, Policy: p}}
	}

	destroyed := (&Reaper{Now: func() time.Time { return now }, Backends: target}).RunOnce(context.Background())
	if len(destroyed) != 1 || destroyed[0] != expired {
		t.Fatalf("reaper destroyed %v, want only %s", destroyed, expired)
	}
	if !n.has(fresh) {
		t.Fatal("reaper destroyed a volume whose grace period had not expired")
	}

	// One second before the grace period is still too early; one second after
	// is not. This is the boundary the whole feature turns on.
	freshRetiredAt := now.Add(-time.Hour) // what justNow renders
	almost := freshRetiredAt.Add(7 * 24 * time.Hour).Add(-time.Second)
	if got := (&Reaper{Now: func() time.Time { return almost }, Backends: target}).RunOnce(context.Background()); len(got) != 0 {
		t.Fatalf("reaper destroyed %v one second before the grace period expired", got)
	}
	if !n.has(fresh) {
		t.Fatal("reaper destroyed a volume one second before its grace period expired")
	}
	if got := (&Reaper{Now: func() time.Time { return almost.Add(2 * time.Second) },
		Backends: target}).RunOnce(context.Background()); len(got) != 1 || got[0] != fresh {
		t.Fatalf("reaper destroyed %v after the grace period expired, want %s", got, fresh)
	}
}

// TestReaperKeepsACloneOriginAndReapsItOnceTheCloneIsGone pins the clone
// behaviour: a retired dataset a live volume was cloned from cannot be
// destroyed, and the reaper must wait for the clone rather than promote it.
func TestReaperKeepsACloneOriginAndReapsItOnceTheCloneIsGone(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)

	n.put(p.Root(), map[string]string{
		volume.OwnerProperty: volume.OwnerValue, volume.GraveyardProperty: volume.GraveyardValue})
	origin := p.Root() + "/20260801T000000Z-pvc-origin"
	n.putRetired(origin, "nas1/nfs/Pool0/k8s/pvc-origin", longAgo)
	n.put("Pool0/k8s/pvc-restored", map[string]string{volume.OwnerProperty: volume.OwnerValue})
	n.clone("Pool0/k8s/pvc-restored", origin)

	client := n.client(t)
	r := &Reaper{Now: func() time.Time { return now },
		Backends: func(context.Context) []Target {
			return []Target{{Name: "nas1", Client: client, Policy: p}}
		}}

	if got := r.RunOnce(context.Background()); len(got) != 0 {
		t.Fatalf("reaper destroyed %v, but a live volume is cloned from it", got)
	}
	if !n.has(origin) {
		t.Fatal("reaper destroyed a clone origin")
	}
	if !n.has("Pool0/k8s/pvc-restored") {
		t.Fatal("the live clone was touched")
	}

	// The clone is deleted; nothing depends on the retired dataset any more.
	n.uncloneAll()
	if got := r.RunOnce(context.Background()); len(got) != 1 || got[0] != origin {
		t.Fatalf("after the clone went away the reaper destroyed %v, want %s", got, origin)
	}
}

// TestReaperDoesNothingWhenProtectionIsOff guards the default: a target with a
// zero policy must produce no calls at all, not an empty sweep.
func TestReaperDoesNothingWhenProtectionIsOff(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/.trash/pvc-1", map[string]string{
		volume.OwnerProperty: volume.OwnerValue, volume.DeletedAtProperty: longAgo})

	r := &Reaper{Now: func() time.Time { return now },
		Backends: func(context.Context) []Target {
			return []Target{{Name: "nas1", Client: n.client(t), Policy: Policy{Pool: "Pool0", Parent: "k8s"}}}
		}}
	if got := r.RunOnce(context.Background()); len(got) != 0 {
		t.Fatalf("a disabled policy destroyed %v", got)
	}
	if n := n.CallsTo("pool.dataset.delete"); n != 0 {
		t.Fatalf("a disabled policy issued %d deletes", n)
	}
}

// TestReapableFallsBackToTheNameTimestamp closes a silent, unbounded space leak.
//
// Retire stamps deleted-at AFTER the rename, deliberately, and treats a
// stamping failure as non-fatal. A controller killed in that window — or one
// whose stamping call simply failed — leaves a dataset inside the graveyard
// with no timestamp, which Reapable refused for ever. The reaper logs that
// refusal at Debug, the orphan report skips graveyard entries, and nothing else
// looks: the space is never reclaimed and no operator is ever told.
//
// The window is narrow — one middleware call wide — and Retire documents the
// outcome as the safe direction to fail in. This is not a failure anyone has
// caught in the act; it is one the code already says it cannot recover from.
//
// The entry NAME carries the same instant, written by the same operation that
// chose the name, and it is only ever consulted for a dataset already confined
// to the graveyard — so this recovers the timestamp without trusting anything
// the property did not already assert.
func TestReapableFallsBackToTheNameTimestamp(t *testing.T) {
	p := Policy{Pool: "Pool0", Parent: "k8s", Graveyard: ".trash", Grace: time.Hour}
	// Everything Retire stamps EXCEPT the timestamp, which is the window.
	props := map[string]string{
		volume.OwnerProperty:       volume.OwnerValue,
		volume.RetiredFromProperty: "nas1/nfs/Pool0/k8s/pvc-1",
	}
	const id = "Pool0/k8s/.trash/20260910T041119Z-pvc-1"
	props[volume.OwnerIDProperty] = id
	ds := dataset(t, id, props, nil)

	retiredAt := time.Date(2026, 9, 10, 4, 11, 19, 0, time.UTC)
	if err := Reapable(ds, p, retiredAt.Add(30*time.Minute)); err == nil {
		t.Fatal("a dataset inside its grace period must not be reapable")
	}
	if err := Reapable(ds, p, retiredAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("past its grace period it must be reapable, got: %v", err)
	}
}

// TestReapableStillRefusesAnUndatedEntry: the fallback must not become a way to
// reap anything whose name carries no timestamp at all.
func TestReapableStillRefusesAnUndatedEntry(t *testing.T) {
	p := Policy{Pool: "Pool0", Parent: "k8s", Graveyard: ".trash", Grace: time.Hour}
	props := map[string]string{
		volume.OwnerProperty:       volume.OwnerValue,
		volume.RetiredFromProperty: "nas1/nfs/Pool0/k8s/pvc-1",
		volume.OwnerIDProperty:     "Pool0/k8s/.trash/handmade",
	}
	ds := dataset(t, "Pool0/k8s/.trash/handmade", props, nil)
	if err := Reapable(ds, p, time.Now()); err == nil {
		t.Fatal("an entry with neither a timestamp property nor a dated name " +
			"must never be reaped — the driver did not put it there")
	}
}

// TestReapableSeparatesWaitingFromStuck: a sweep must be able to tell a dataset
// that is merely waiting out its grace period from one no sweep will ever
// reclaim. Only the first is routine, and only the first belongs at Debug —
// the orphan report skips graveyard entries, so Debug was the sole place a
// permanently stuck dataset appeared, and Debug is off by default.
func TestReapableSeparatesWaitingFromStuck(t *testing.T) {
	p := Policy{Pool: "Pool0", Parent: "k8s", Graveyard: ".trash", Grace: time.Hour}
	now := time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC)

	waiting := dataset(t, "Pool0/k8s/.trash/20260910T045500Z-pvc-1",
		retiredProps("2026-09-10T04:55:00Z"), nil)
	err := Reapable(waiting, p, now)
	if !errors.Is(err, errStillInGrace) {
		t.Fatalf("a dataset inside its grace period must report as waiting, got: %v", err)
	}

	// Not driver-owned: no later sweep resolves this on its own.
	stuck := dataset(t, "Pool0/k8s/.trash/20260910T030000Z-pvc-2",
		map[string]string{volume.DeletedAtProperty: "2026-09-10T03:00:00Z"}, nil)
	err = Reapable(stuck, p, now)
	if err == nil {
		t.Fatal("an unowned graveyard dataset must never be reaped")
	}
	if errors.Is(err, errStillInGrace) {
		t.Fatalf("a permanently stuck dataset must not report as merely waiting: %v", err)
	}
}
