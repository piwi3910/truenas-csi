package truenas

import (
	"context"
	"net"
	"strings"
)

// RenewAgeUnknown is the NFSClient.RenewAgeSeconds value for a client whose
// liveness the appliance does not report — every NFSv3 client, because rmtab
// records a mount, not a heartbeat.
const RenewAgeUnknown = -1

// ISCSISession is one live iSCSI session as the appliance sees it.
//
// This is the appliance-side answer to "is that node still attached?", which is
// the only trustworthy one during a fence: the node's own kernel may be wedged,
// unreachable, or lying, but the target knows who is holding a connection.
//
// UNVERIFIED: iscsi.global.sessions was confirmed on hardware to exist and to
// answer, but the appliance had nothing attached, so it returned an empty list.
// These field names come from the published schema at
// https://192.168.10.253/api/docs/current/api_methods_iscsi.global.sessions.html
// (initiator, initiator_addr, target, target_alias) and still need one run with
// a LUN actually attached to confirm a populated element.
type ISCSISession struct {
	// Initiator is the initiator's IQN, matching the node's InitiatorName.
	Initiator string
	// InitiatorAddr is the initiator's IP address. It is the fallback identity
	// when a node's IQN is unknown, and the cross-check when it is known.
	InitiatorAddr string
	// Target is the target's IQN.
	Target string
	// TargetName is the target's alias — the short name the driver created the
	// target with, without the iscsi.global basename prefix.
	TargetName string
}

// NFSClient is one client currently holding an NFS mount on the appliance.
type NFSClient struct {
	// Address is the client's IP address, without a port.
	Address string
	// RenewAgeSeconds is how long ago the client last renewed its NFSv4 lease,
	// or RenewAgeUnknown when the appliance does not say.
	//
	// This is the liveness signal a fence turns on: a client that renewed two
	// seconds ago is alive and must not be cut, while one that last renewed
	// minutes ago is holding state it may never come back for.
	RenewAgeSeconds int
}

// ISCSISessions lists every live iSCSI session, across all targets.
//
// Unlike ISCSISessionCount this keeps the initiator identity, because a fencing
// decision has to know WHICH node is still attached, not how many are.
func (c *Ops) ISCSISessions(ctx context.Context) ([]ISCSISession, error) {
	var out []iscsiSession
	if err := c.CallJSON(ctx, &out, "iscsi.global.sessions"); err != nil {
		return nil, err
	}
	sessions := make([]ISCSISession, 0, len(out))
	for _, s := range out {
		sessions = append(sessions, ISCSISession{
			Initiator:     s.Initiator,
			InitiatorAddr: s.InitiatorAddr,
			Target:        s.Target,
			TargetName:    s.TargetAlias,
		})
	}
	return sessions, nil
}

type iscsiSession struct {
	Initiator     string `json:"initiator"`
	InitiatorAddr string `json:"initiator_addr"`
	Target        string `json:"target"`
	TargetAlias   string `json:"target_alias"`
}

// ISCSIClientCount is the appliance's own count of attached iSCSI clients.
//
// It is a cheap health signal — one call returning a bare integer — and is not
// the same number as ISCSISessionCount: one client may hold several sessions.
func (c *Ops) ISCSIClientCount(ctx context.Context) (int, error) {
	var n int
	if err := c.CallJSON(ctx, &n, "iscsi.global.client_count"); err != nil {
		return 0, err
	}
	return n, nil
}

// NFSClientCount is the appliance's own count of connected NFS clients.
//
// Cheap enough to poll for metrics, and documented as possibly inaccurate while
// NFSv3 is in use because rmtab entries go stale. Use NFSClients when the
// identity of a client matters.
func (c *Ops) NFSClientCount(ctx context.Context) (int, error) {
	var n int
	if err := c.CallJSON(ctx, &n, "nfs.client_count"); err != nil {
		return 0, err
	}
	return n, nil
}

// NFSClients lists the clients currently holding an NFS mount.
//
// nfs.get_nfs4_clients is the real source on 25.10: it reads
// /proc/fs/nfsd/clients and was confirmed on hardware to list live mounts with
// an address and a lease age. nfs.get_nfs3_clients reads rmtab and returned an
// EMPTY list on the same box, because its exports are v4 — it is still called
// so that a genuinely v3 client is not invisible, but it is the lesser source
// and carries no liveness signal.
//
// Two limitations a caller must design around:
//
//   - Neither source is authoritative about departure. rmtab is documented as
//     possibly stale, "a limitation of the NFSv3 protocol", and an NFSv4 client
//     lingers in "courtesy" state for 90 seconds and "expirable" for 24 hours
//     after it stops talking. Clients in every state are therefore reported, and
//     RenewAgeSeconds is what separates them: for a fence the safe error is
//     believing a departed client is still present, never the reverse.
//   - An address here is the client's, not a volume's. Neither method says
//     which export an NFSv4 client is using, so this answers "is that node
//     mounting anything from this appliance", not "is it mounting this volume".
//
// An error from either source fails the whole call rather than returning half
// an answer, because a short list would read as "that node has let go".
//
// UNVERIFIED: the address text for an IPv6 client. Every observed value is IPv4
// "host:port" (e.g. "192.168.10.102:666"); a hardware run with an IPv6 NFSv4
// mount should confirm it is bracketed host:port, which is what the parsing
// here assumes.
func (c *Ops) NFSClients(ctx context.Context) ([]NFSClient, error) {
	var v4 []nfs4Client
	if err := c.CallJSON(ctx, &v4, "nfs.get_nfs4_clients"); err != nil {
		return nil, err
	}
	var v3 []nfs3Client
	if err := c.CallJSON(ctx, &v3, "nfs.get_nfs3_clients"); err != nil {
		return nil, err
	}

	index := map[string]int{}
	clients := make([]NFSClient, 0, len(v3)+len(v4))
	add := func(addr string, renew int) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		if i, ok := index[addr]; ok {
			// One node reached over both protocols, or twice in rmtab, is one
			// client. Keep the freshest liveness evidence for it.
			if renew != RenewAgeUnknown &&
				(clients[i].RenewAgeSeconds == RenewAgeUnknown || renew < clients[i].RenewAgeSeconds) {
				clients[i].RenewAgeSeconds = renew
			}
			return
		}
		index[addr] = len(clients)
		clients = append(clients, NFSClient{Address: addr, RenewAgeSeconds: renew})
	}
	for _, client := range v4 {
		add(hostOnly(client.Info.Address), client.Info.RenewAgeSeconds)
	}
	for _, client := range v3 {
		add(client.IP, RenewAgeUnknown)
	}
	return clients, nil
}

type nfs3Client struct {
	IP     string `json:"ip"`
	Export string `json:"export"`
}

// nfs4Client mirrors /proc/fs/nfsd/clients. Only the fields a fence needs are
// decoded: the rest of "info" is free-form kernel text and "states" carries
// per-open-file detail this driver has no use for.
//
// The tags are not decoration. Several keys CONTAIN SPACES — "seconds from last
// renew", "minor version", "callback state" — so nothing here can be matched by
// Go's default field-name mapping.
type nfs4Client struct {
	ID   string `json:"id"`
	Info struct {
		Address         string `json:"address"` // "host:port"
		Status          string `json:"status"`  // confirmed | courtesy | expirable
		RenewAgeSeconds int    `json:"seconds from last renew"`
	} `json:"info"`
}

// hostOnly strips the port from an "ip:port" pair, leaving a bare address alone.
func hostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
