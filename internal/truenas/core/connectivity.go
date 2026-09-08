package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// ErrNotSupported means a query has no CORE equivalent this client can make
// safely.
//
// It is deliberately an error rather than an empty answer. These three queries
// exist to decide whether a node is still holding a volume; "no sessions" and
// "I cannot tell" are opposite conclusions, and returning the first for the
// second would let a fencing controller cut a node that is still writing.
var ErrNotSupported = errors.New("not supported on TrueNAS CORE")

// ReportingGetData is not available on CORE.
//
// UNVERIFIED: CORE's REST v2 does expose /reporting/get_data, but its request
// body is a single JSON object ({"graphs": [...], "unit": ..., "page": ...}),
// not the two positional parameters SCALE takes. The generic method router in
// this package would post only the graph list and silently drop the time range,
// producing data for the WRONG window — which is worse than no data. Wiring
// this up needs a hardware run against a CORE appliance to capture the exact
// body and response shape, at which point it becomes a dedicated route rather
// than a promoted Ops method.
func (c *Client) ReportingGetData(context.Context, []truenas.ReportingQuery, time.Time, time.Time) ([]truenas.ReportingSeries, error) {
	return nil, fmt.Errorf("reporting.get_data: %w", ErrNotSupported)
}

// ISCSISessions is not available on CORE.
//
// UNVERIFIED: the method name exists in CORE's namespace, but the router here
// maps an unrecognised trailing segment onto POST <collection>/<op>, and a
// session listing is a GET. Rather than guess the verb — a POST to a session
// endpoint is not obviously side-effect free — this refuses. A hardware run on
// CORE should confirm the verb, path and field names of the session listing.
func (c *Client) ISCSISessions(context.Context) ([]truenas.ISCSISession, error) {
	return nil, fmt.Errorf("iscsi.global.sessions: %w", ErrNotSupported)
}

// NVMeSessions is not available on CORE.
//
// This one is not a routing problem: TrueNAS CORE is FreeBSD-based and has no
// nvmet namespace at all, so there is nothing to route to. It refuses for the
// same reason as the rest — a fencing controller reads an empty session list as
// "that node has let go", and CORE cannot honestly say that.
//
// UNVERIFIED: that no CORE release exposes any NVMe-oF target surface. The
// hardware check is core.get_methods on a CORE appliance, looking for an nvmet
// (or ctld NVMe) namespace.
func (c *Client) NVMeSessions(context.Context) ([]truenas.NVMeSession, error) {
	return nil, fmt.Errorf("nvmet.global.sessions: %w", ErrNotSupported)
}

// ISCSIClientCount is not available on CORE.
//
// UNVERIFIED: same routing problem as ISCSISessions — the generic router would
// POST to a read endpoint. A CORE hardware run should confirm the verb and path
// of iscsi/global/client_count.
func (c *Client) ISCSIClientCount(context.Context) (int, error) {
	return 0, fmt.Errorf("iscsi.global.client_count: %w", ErrNotSupported)
}

// NFSClientCount is not available on CORE.
//
// UNVERIFIED: as above for nfs/client_count.
func (c *Client) NFSClientCount(context.Context) (int, error) {
	return 0, fmt.Errorf("nfs.client_count: %w", ErrNotSupported)
}

// NFSClients is not available on CORE.
//
// UNVERIFIED: nfs.get_nfs3_clients and nfs.get_nfs4_clients read Linux-specific
// state (rmtab and /proc/fs/nfsd/clients); CORE is FreeBSD-based and its NFS
// server keeps neither in that form. A hardware run on CORE should establish
// whether any equivalent listing exists at all before this stops refusing.
func (c *Client) NFSClients(context.Context) ([]truenas.NFSClient, error) {
	return nil, fmt.Errorf("nfs clients: %w", ErrNotSupported)
}
