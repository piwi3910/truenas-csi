package smb

import (
	"context"
	"sort"
	"strings"
)

// DenyAll is the hostsdeny entry that makes an SMB share's allow list
// authoritative.
//
// Samba, and therefore TrueNAS, treats an empty hostsallow as "no restriction".
// The appliance's own schema names the remedy: "The keyword ALL or the netmask
// 0.0.0.0/0 may be used to deny all by default." So every driver-managed share
// carries hostsdeny=["ALL"], and hostsallow is then the exhaustive list of who
// may reach it. Emptying hostsallow denies everyone — which is the fence —
// rather than admitting everyone.
//
// Nothing may remove it. Every write of a share's access lists goes through
// accessOptions, which puts it back.
// purposeDefaultShare is the share preset this driver creates.
//
// The middleware requires `purpose` alongside `options`, and every share this
// driver creates carries options because it is created fenced. DEFAULT_SHARE is
// the minimal preset; LEGACY_SHARE would drag in a large option set the driver
// does not manage and would then have to preserve on every access update.
const purposeDefaultShare = "DEFAULT_SHARE"

const DenyAll = "ALL"

// Option keys inside sharing.smb's nested `options` object.
const (
	optHostsAllow = "hostsallow"
	optHostsDeny  = "hostsdeny"
)

// smbShare is the subset of sharing.smb this package needs. It lives here
// rather than in internal/truenas because nothing else in the driver speaks SMB.
//
// Options is kept as a raw map on purpose. It is a per-purpose object — a
// LEGACY_SHARE carries a dozen keys a DEFAULT_SHARE does not — and an update
// replaces it wholesale, so writing a freshly built object containing only the
// host lists would silently reset every sibling setting the operator chose.
type smbShare struct {
	ID      int            `json:"id"`
	Path    string         `json:"path"`
	Name    string         `json:"name"`
	Options map[string]any `json:"options"`
}

// hostsAllow reads the share's current allow list.
func (s *smbShare) hostsAllow() []string {
	if s == nil {
		return nil
	}
	raw, _ := s.Options[optHostsAllow].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if str, ok := v.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	return out
}

// accessOptions returns the share's options with the access lists replaced.
//
// The caller's options are copied rather than mutated, and every key it did not
// ask about is carried through untouched.
func accessOptions(current map[string]any, allow []string) map[string]any {
	out := make(map[string]any, len(current)+2)
	for k, v := range current {
		out[k] = v
	}
	out[optHostsAllow] = normaliseHosts(allow)
	out[optHostsDeny] = []string{DenyAll}
	return out
}

// normaliseHosts deduplicates and orders an allow list. An empty result is
// correct and meaningful here — with hostsdeny=ALL it denies everyone.
func normaliseHosts(hosts []string) []string {
	set := map[string]bool{}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// withoutHosts removes a node's addresses from an allow list.
func withoutHosts(current, remove []string) []string {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[strings.TrimSpace(r)] = true
	}
	kept := make([]string, 0, len(current))
	for _, h := range current {
		if !drop[h] {
			kept = append(kept, h)
		}
	}
	return normaliseHosts(kept)
}

func (b *Backend) shareByPath(ctx context.Context, path string) (*smbShare, error) {
	var out []smbShare
	err := b.c.CallJSON(ctx, &out, "sharing.smb.query",
		[]any{[]any{"path", "=", path}}, map[string]any{})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// setHostsAllow rewrites a share's access lists, preserving every other option.
//
// UNVERIFIED: that sharing.smb.update accepts the `options` object read back
// from sharing.smb.query unchanged for every `purpose`. Settling it needs one
// update against a LEGACY_SHARE on real hardware; DEFAULT_SHARE is what this
// driver creates and is the only shape covered by the integration suite.
func (b *Backend) setHostsAllow(ctx context.Context, share *smbShare, allow []string) error {
	return b.c.CallJSON(ctx, nil, "sharing.smb.update", share.ID, map[string]any{
		"options": accessOptions(share.Options, allow),
	})
}
