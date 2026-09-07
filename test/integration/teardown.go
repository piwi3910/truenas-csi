// Package integration exercises the driver against a real TrueNAS appliance.
package integration

import (
	"context"
	"fmt"
	"sort"

	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// State is a snapshot of everything on the appliance the driver can create.
//
// The suite compares a State taken before the run with one taken after: any
// difference is an object the driver leaked, and on an appliance holding real
// user data a leak is the first symptom of a cleanup path that does not work.
type State struct {
	Datasets       []string
	Snapshots      []string
	NFSShares      []string
	ISCSIExtents   []string
	ISCSITargets   []string
	ISCSIPortals   []int
	TargetExtents  []int
	countedPrefix  string
	countedFilters bool
}

// SnapshotState records the appliance's current objects under a dataset prefix.
func SnapshotState(ctx context.Context, c *truenas.Client, prefix string) (State, error) {
	var s State
	s.countedPrefix = prefix
	s.countedFilters = true

	datasets, err := c.DatasetList(ctx, prefix)
	if err != nil {
		return s, fmt.Errorf("list datasets: %w", err)
	}
	for _, d := range datasets {
		s.Datasets = append(s.Datasets, d.ID)
	}
	snaps, err := c.SnapshotList(ctx, prefix)
	if err != nil {
		return s, fmt.Errorf("list snapshots: %w", err)
	}
	for _, sn := range snaps {
		s.Snapshots = append(s.Snapshots, sn.ID)
	}
	var shares []struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
	}
	if err := c.CallJSON(ctx, &shares, "sharing.nfs.query"); err != nil {
		return s, fmt.Errorf("list nfs shares: %w", err)
	}
	for _, sh := range shares {
		s.NFSShares = append(s.NFSShares, fmt.Sprintf("%d:%s", sh.ID, sh.Path))
	}
	var extents []truenas.ISCSIExtent
	if err := c.CallJSON(ctx, &extents, "iscsi.extent.query"); err != nil {
		return s, fmt.Errorf("list extents: %w", err)
	}
	for _, e := range extents {
		s.ISCSIExtents = append(s.ISCSIExtents, fmt.Sprintf("%d:%s", e.ID, e.Name))
	}
	var targets []truenas.ISCSITarget
	if err := c.CallJSON(ctx, &targets, "iscsi.target.query"); err != nil {
		return s, fmt.Errorf("list targets: %w", err)
	}
	for _, tg := range targets {
		s.ISCSITargets = append(s.ISCSITargets, fmt.Sprintf("%d:%s", tg.ID, tg.Name))
	}
	portals, err := c.PortalList(ctx)
	if err != nil {
		return s, fmt.Errorf("list portals: %w", err)
	}
	for _, p := range portals {
		s.ISCSIPortals = append(s.ISCSIPortals, p.ID)
	}
	var tes []truenas.ISCSITargetExtent
	if err := c.CallJSON(ctx, &tes, "iscsi.targetextent.query"); err != nil {
		return s, fmt.Errorf("list targetextents: %w", err)
	}
	for _, te := range tes {
		s.TargetExtents = append(s.TargetExtents, te.ID)
	}

	sort.Strings(s.Datasets)
	sort.Strings(s.Snapshots)
	sort.Strings(s.NFSShares)
	sort.Strings(s.ISCSIExtents)
	sort.Strings(s.ISCSITargets)
	sort.Ints(s.ISCSIPortals)
	sort.Ints(s.TargetExtents)
	return s, nil
}

// Diff lists everything present in other but not in s — the leaked objects.
func (s State) Diff(other State) []string {
	var out []string
	out = append(out, addedStrings("dataset", s.Datasets, other.Datasets)...)
	out = append(out, addedStrings("snapshot", s.Snapshots, other.Snapshots)...)
	out = append(out, addedStrings("nfs share", s.NFSShares, other.NFSShares)...)
	out = append(out, addedStrings("iscsi extent", s.ISCSIExtents, other.ISCSIExtents)...)
	out = append(out, addedStrings("iscsi target", s.ISCSITargets, other.ISCSITargets)...)
	out = append(out, addedInts("iscsi portal", s.ISCSIPortals, other.ISCSIPortals)...)
	out = append(out, addedInts("targetextent", s.TargetExtents, other.TargetExtents)...)
	sort.Strings(out)
	return out
}

func addedStrings(kind string, before, after []string) []string {
	seen := map[string]struct{}{}
	for _, b := range before {
		seen[b] = struct{}{}
	}
	var out []string
	for _, a := range after {
		if _, ok := seen[a]; !ok {
			out = append(out, kind+" "+a)
		}
	}
	return out
}

func addedInts(kind string, before, after []int) []string {
	seen := map[int]struct{}{}
	for _, b := range before {
		seen[b] = struct{}{}
	}
	var out []string
	for _, a := range after {
		if _, ok := seen[a]; !ok {
			out = append(out, fmt.Sprintf("%s %d", kind, a))
		}
	}
	return out
}
