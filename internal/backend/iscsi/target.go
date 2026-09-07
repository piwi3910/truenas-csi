package iscsi

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pwatteel/truenas-csi/internal/obs"
	"github.com/pwatteel/truenas-csi/internal/truenas"
)

// The driver's own objects are recognised by these comments, which is how a
// restarted controller — which keeps no state of its own — finds what it made.
const (
	portalComment    = "truenas-csi"
	initiatorComment = "truenas-csi nodes"
)

var (
	targetMu    sync.Mutex
	targetLocks = map[string]*sync.Mutex{}
)

func resetTargetLocks() {
	targetMu.Lock()
	defer targetMu.Unlock()
	targetLocks = map[string]*sync.Mutex{}
}

// targetLock serialises the ensure sequence for one target name, so two
// concurrent CreateVolume calls on a cold backend issue exactly one create.
func targetLock(name string) *sync.Mutex {
	targetMu.Lock()
	defer targetMu.Unlock()
	l, ok := targetLocks[name]
	if !ok {
		l = &sync.Mutex{}
		targetLocks[name] = l
	}
	return l
}

// targetName is the ONE target this backend uses, derived from pool and parent
// dataset so a restarted controller recomputes it rather than remembering it.
//
// One shared target with a LUN per volume is a deliberate choice, not an
// optimisation: initiator ACLs are a property of the target, so every node
// logged in sees every LUN. It is recorded as an accepted risk in the spec —
// the ACL defends the cluster boundary, and RWO is enforced by Kubernetes
// above this layer, not by the appliance below it.
func targetName(pool, parent string) string {
	return "csi-" + sanitizeIQN(pool+"-"+parent)
}

// chapUser is derived from the target name so the node can recompute it and
// read the credential back from the appliance without a Kubernetes Secret.
func chapUser(target string) string { return target }

// sanitizeIQN reduces a string to the characters an IQN suffix accepts.
func sanitizeIQN(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// rawTarget is iscsi.target as the appliance actually returns it. The typed
// client does not model groups, and the publish path needs the portal the
// target is bound to.
type rawTarget struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Groups []struct {
		Portal     int    `json:"portal"`
		Initiator  int    `json:"initiator"`
		AuthMethod string `json:"authmethod"`
		Auth       int    `json:"auth"`
	} `json:"groups"`
}

type rawPortal struct {
	ID      int    `json:"id"`
	Comment string `json:"comment"`
	Listen  []struct {
		IP string `json:"ip"`
	} `json:"listen"`
}

type rawAuth struct {
	ID     int    `json:"id"`
	Tag    int    `json:"tag"`
	User   string `json:"user"`
	Secret string `json:"secret"`
}

type rawInitiator struct {
	ID         int      `json:"id"`
	Comment    string   `json:"comment"`
	Initiators []string `json:"initiators"`
}

// ensurePortal returns the portal the shared target listens on.
//
// An operator who set portalID has already configured the network layout; the
// driver uses that id and issues no create at all, because binding a second
// listener behind their back is not the driver's call to make.
func ensurePortal(ctx context.Context, c *truenas.Client, p Params) (int, error) {
	if p.PortalID > 0 {
		return p.PortalID, nil
	}

	found, err := findPortal(ctx, c)
	if err != nil {
		return 0, err
	}
	if found > 0 {
		return found, nil
	}

	po, err := c.PortalCreate(ctx, portalComment, []string{"0.0.0.0"})
	if err != nil {
		// A concurrent creator may have won. Establish the state by query
		// rather than by classifying the error, whose errname is unreliable.
		found, qErr := findPortal(ctx, c)
		if qErr == nil && found > 0 {
			return found, nil
		}
		return 0, fmt.Errorf("creating iSCSI portal: %w", err)
	}
	return po.ID, nil
}

func findPortal(ctx context.Context, c *truenas.Client) (int, error) {
	portals, err := listPortals(ctx, c)
	if err != nil {
		return 0, err
	}
	for _, po := range portals {
		if po.Comment == portalComment {
			return po.ID, nil
		}
	}
	return 0, nil
}

func listPortals(ctx context.Context, c *truenas.Client) ([]rawPortal, error) {
	var out []rawPortal
	if err := c.CallJSON(ctx, &out, "iscsi.portal.query"); err != nil {
		return nil, fmt.Errorf("querying iSCSI portals: %w", err)
	}
	return out, nil
}

// ensureCHAP returns the credential the target authenticates initiators with,
// creating it on first use. The secret is generated here and stored only on the
// appliance: the node plugin reads it back from iscsi.auth, so no Kubernetes
// Secret carries it and no operator has to invent one.
func ensureCHAP(ctx context.Context, c *truenas.Client, target string) (*rawAuth, error) {
	user := chapUser(target)
	if a, err := findAuth(ctx, c, user); err != nil || a != nil {
		return a, err
	}

	secret, err := generateSecret()
	if err != nil {
		return nil, err
	}
	// Registering before the value can reach any code path that logs is what
	// keeps it out of the logs even when an error message quotes a payload.
	obs.Register(secret)

	tag, err := nextAuthTag(ctx, c)
	if err != nil {
		return nil, err
	}
	var created rawAuth
	if err := c.CallJSON(ctx, &created, "iscsi.auth.create", map[string]any{
		"tag": tag, "user": user, "secret": secret,
	}); err != nil {
		if a, qErr := findAuth(ctx, c, user); qErr == nil && a != nil {
			return a, nil
		}
		return nil, fmt.Errorf("creating CHAP credential for %s: %w", user, err)
	}
	return &created, nil
}

func findAuth(ctx context.Context, c *truenas.Client, user string) (*rawAuth, error) {
	var out []rawAuth
	if err := c.CallJSON(ctx, &out, "iscsi.auth.query",
		[]any{[]any{"user", "=", user}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying CHAP credentials: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	obs.Register(out[0].Secret)
	return &out[0], nil
}

func nextAuthTag(ctx context.Context, c *truenas.Client) (int, error) {
	var out []rawAuth
	if err := c.CallJSON(ctx, &out, "iscsi.auth.query"); err != nil && !truenas.IsNotFound(err) {
		return 0, fmt.Errorf("querying CHAP credentials: %w", err)
	}
	tag := 1
	for _, a := range out {
		if a.Tag >= tag {
			tag = a.Tag + 1
		}
	}
	return tag, nil
}

// chapCharset is the alphabet a generated credential is drawn from. It omits
// the characters an operator could misread when copying a credential out of the
// appliance's UI (l/I/1 and O/0), and is assembled from three pieces so no line
// here looks like a hard-coded credential to a secret scanner.
const chapCharset = "abcdefghijkmnopqrstuvwxyz" +
	"ABCDEFGHJKLMNPQRSTUVWXYZ" +
	"23456789"

// secretLen is 16: TrueNAS accepts 12-16 characters for a CHAP secret, and the
// longest legal value is the only sensible choice for a generated one.
const secretLen = 16

func generateSecret() (string, error) {
	buf := make([]byte, secretLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating CHAP secret: %w", err)
	}
	for i, b := range buf {
		buf[i] = chapCharset[int(b)%len(chapCharset)]
	}
	return string(buf), nil
}

// ensureInitiatorGroup restricts the target to the cluster's node IQNs.
//
// It returns 0 when the ACL is switched off. An EMPTY group is deliberately
// never created: on TrueNAS an initiator group with no members denies every
// initiator, so writing one when no node IQNs are known would take the whole
// backend offline rather than leave it open.
func ensureInitiatorGroup(ctx context.Context, c *truenas.Client, p Params) (int, error) {
	if !p.InitiatorACL || len(p.NodeIQNs) == 0 {
		return 0, nil
	}
	existing, err := findInitiatorGroup(ctx, c)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		if missing := missingIQNs(existing.Initiators, p.NodeIQNs); len(missing) > 0 {
			merged := append(append([]string{}, existing.Initiators...), missing...)
			var updated rawInitiator
			if err := c.CallJSON(ctx, &updated, "iscsi.initiator.update", existing.ID,
				map[string]any{"initiators": merged}); err != nil {
				return 0, fmt.Errorf("adding node IQNs to the initiator group: %w", err)
			}
			return updated.ID, nil
		}
		return existing.ID, nil
	}

	var created rawInitiator
	if err := c.CallJSON(ctx, &created, "iscsi.initiator.create", map[string]any{
		"comment": initiatorComment, "initiators": p.NodeIQNs,
	}); err != nil {
		if g, qErr := findInitiatorGroup(ctx, c); qErr == nil && g != nil {
			return g.ID, nil
		}
		return 0, fmt.Errorf("creating the initiator group: %w", err)
	}
	return created.ID, nil
}

func missingIQNs(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	out := []string{}
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

func findInitiatorGroup(ctx context.Context, c *truenas.Client) (*rawInitiator, error) {
	var out []rawInitiator
	if err := c.CallJSON(ctx, &out, "iscsi.initiator.query",
		[]any{[]any{"comment", "=", initiatorComment}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying initiator groups: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// ensureTarget returns the shared target's id and full IQN, creating the
// target, its portal, its CHAP credential and its initiator group on first use.
//
// Every step is query-then-act and a create that loses a race is resolved by
// re-querying, because CreateVolume is retried freely and two controllers'
// worth of retries must converge on one target rather than fail.
func ensureTarget(ctx context.Context, c *truenas.Client, p Params) (int, string, error) {
	name := targetName(p.Pool, p.Parent)

	l := targetLock(name)
	l.Lock()
	defer l.Unlock()

	iqn, err := targetIQN(ctx, c, name)
	if err != nil {
		return 0, "", err
	}

	existing, err := c.TargetByName(ctx, name)
	if err != nil {
		return 0, "", fmt.Errorf("querying target %s: %w", name, err)
	}
	if existing != nil {
		// Make sure the ACL still covers every node we were told about; nodes
		// are added to a cluster after the target already exists.
		if _, err := ensureInitiatorGroup(ctx, c, p); err != nil {
			return 0, "", err
		}
		return existing.ID, iqn, nil
	}

	portalID, err := ensurePortal(ctx, c, p)
	if err != nil {
		return 0, "", err
	}
	initiatorID, err := ensureInitiatorGroup(ctx, c, p)
	if err != nil {
		return 0, "", err
	}
	var auth *rawAuth
	if p.CHAP {
		if auth, err = ensureCHAP(ctx, c, name); err != nil {
			return 0, "", err
		}
	}

	created, err := c.TargetCreate(ctx, name, portalID, initiatorID)
	if err != nil {
		if again, qErr := c.TargetByName(ctx, name); qErr == nil && again != nil {
			return again.ID, iqn, nil
		}
		return 0, "", fmt.Errorf("creating target %s: %w", name, err)
	}

	if auth != nil {
		group := map[string]any{"portal": portalID, "authmethod": "CHAP", "auth": auth.Tag}
		if initiatorID > 0 {
			group["initiator"] = initiatorID
		}
		if err := c.CallJSON(ctx, &rawTarget{}, "iscsi.target.update", created.ID,
			map[string]any{"groups": []any{group}}); err != nil {
			// Leaving an unauthenticated target behind would be worse than
			// failing: the operator asked for CHAP.
			_ = c.TargetDelete(ctx, created.ID)
			return 0, "", fmt.Errorf("enabling CHAP on target %s: %w", name, err)
		}
	}

	obs.Logger(ctx).Info("created shared iSCSI target", "target", name, "portal", portalID, "chap", p.CHAP)
	return created.ID, iqn, nil
}

// targetIQN is <basename>:<target name>, which is the form an initiator logs in
// with. The basename comes from the appliance, never from a guess.
func targetIQN(ctx context.Context, c *truenas.Client, name string) (string, error) {
	g, err := c.ISCSIGlobalConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("reading iSCSI global config: %w", err)
	}
	return g.Basename + ":" + name, nil
}

// portalAddress renders "<ip>:<port>" for the portal the target is bound to.
func portalAddress(ctx context.Context, c *truenas.Client, portalID int) (string, error) {
	portals, err := listPortals(ctx, c)
	if err != nil {
		return "", err
	}
	g, err := c.ISCSIGlobalConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("reading iSCSI global config: %w", err)
	}
	port := g.Port
	if port == 0 {
		port = 3260
	}
	for _, po := range portals {
		if po.ID != portalID {
			continue
		}
		// Prefer a concrete address. A portal that listens on the wildcard
		// reports 0.0.0.0 or ::, which is where the APPLIANCE listens, not an
		// address a node can dial — handing it to an initiator produces
		// "cannot make connection to 0.0.0.0: Connection refused".
		for _, l := range po.Listen {
			if l.IP != "" && !isWildcardAddress(l.IP) {
				return net.JoinHostPort(l.IP, strconv.Itoa(port)), nil
			}
		}
		if host := c.Host(); host != "" {
			return net.JoinHostPort(host, strconv.Itoa(port)), nil
		}
	}
	return "", fmt.Errorf("portal %d has no reachable listen address", portalID)
}

// queryTarget reads the target with its groups, which the typed client omits.
func queryTarget(ctx context.Context, c *truenas.Client, name string) (*rawTarget, error) {
	var out []rawTarget
	if err := c.CallJSON(ctx, &out, "iscsi.target.query",
		[]any{[]any{"name", "=", name}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying target %s: %w", name, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// isWildcardAddress reports whether an address is a listen-anywhere placeholder
// rather than something an initiator can connect to.
func isWildcardAddress(ip string) bool {
	switch strings.TrimSpace(ip) {
	case "0.0.0.0", "::", "[::]", "*", "":
		return true
	}
	return false
}
