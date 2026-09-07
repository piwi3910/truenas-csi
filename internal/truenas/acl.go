package truenas

import (
	"context"
	"fmt"
)

// ACL tag names for the three special NFSv4 principals. They carry no numeric
// id, which the middleware expects to be -1 rather than absent.
const (
	ACLTagOwner    = "owner@"
	ACLTagGroup    = "group@"
	ACLTagEveryone = "everyone@"

	// ACLNoID is the id for a tag that names a principal symbolically.
	ACLNoID = -1
)

// ACL entry types.
const (
	ACLTypeAllow = "ALLOW"
	ACLTypeDeny  = "DENY"
)

// Basic permission and flag presets the middleware accepts in place of the full
// per-bit NFSv4 sets. Using the presets keeps the driver out of the business of
// assembling 14 individual permission bits correctly.
const (
	ACLPermFullControl = "FULL_CONTROL"
	ACLPermModify      = "MODIFY"
	ACLPermRead        = "READ"
	ACLPermTraverse    = "TRAVERSE"
	ACLPermNone        = "NOPERM"

	ACLFlagInherit = "INHERIT"
	ACLFlagNone    = "NOINHERIT"
)

// ACLEntry is one access control entry of an NFSv4 ACL.
//
// SMB datasets are created with share_type SMB, which gives them mode 0770 and
// an NFSv4 ACL (filesystem.stat reports acl:true) — a different permission model
// from a plain dataset's 0755 with no ACL. Setting permissions on one with
// filesystem.setperm would strip that ACL instead of configuring it, so the SMB
// path uses filesystem.setacl and this type describes its entries.
type ACLEntry struct {
	Tag   string            `json:"tag"`
	ID    int               `json:"id"`
	Type  string            `json:"type"`
	Perms map[string]string `json:"perms"`
	Flags map[string]string `json:"flags"`
}

// AllowEntry builds an ALLOW entry for a symbolic principal from the basic
// permission and flag presets.
func AllowEntry(tag, perm, flag string) ACLEntry {
	return ACLEntry{
		Tag:   tag,
		ID:    ACLNoID,
		Type:  ACLTypeAllow,
		Perms: map[string]string{"BASIC": perm},
		Flags: map[string]string{"BASIC": flag},
	}
}

// SetACL replaces the NFSv4 ACL on a path and sets its owner.
//
// Like filesystem.setperm this is a JOB: it returns an integer job id which must
// be polled through core.get_jobs until SUCCESS, FAILED or ABORTED. The two
// calls are the only job-based methods the driver makes.
func (c *Client) SetACL(ctx context.Context, path string, dacl []ACLEntry, uid, gid int) error {
	if len(dacl) == 0 {
		return fmt.Errorf("filesystem.setacl %s: refusing to apply an empty ACL", path)
	}
	var jobID int
	err := c.CallJSON(ctx, &jobID, "filesystem.setacl", map[string]any{
		"path": path,
		"dacl": dacl,
		"uid":  uid,
		"gid":  gid,
		"options": map[string]any{
			"recursive": true,
			"traverse":  false,
			// stripacl false is what keeps this an ACL update rather than a
			// conversion back to a bare POSIX mode.
			"stripacl": false,
		},
	})
	if err != nil {
		return fmt.Errorf("filesystem.setacl %s: %w", path, err)
	}
	return c.waitForJob(ctx, jobID)
}
