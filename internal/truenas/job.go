package truenas

import (
	"context"
	"fmt"
	"time"
)

const (
	jobPollInterval = 300 * time.Millisecond
	jobTimeout      = 2 * time.Minute
)

type jobState struct {
	ID    int    `json:"id"`
	State string `json:"state"`
	Error string `json:"error"`
}

// waitForJob polls a middleware job to a terminal state.
//
// Only a handful of methods are jobs — of the CSI-relevant surface, only
// filesystem.setperm. Everything else (dataset create/update/delete, snapshot
// create/clone/delete, the iscsi and sharing calls) is synchronous, which is why
// there is no general async machinery here.
func (c *Ops) waitForJob(ctx context.Context, id int) error {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()

	for {
		var jobs []jobState
		if err := c.CallJSON(ctx, &jobs, "core.get_jobs",
			[]any{[]any{"id", "=", id}}, map[string]any{}); err != nil {
			return fmt.Errorf("polling job %d: %w", id, err)
		}
		if len(jobs) > 0 {
			switch jobs[0].State {
			case "SUCCESS":
				return nil
			case "FAILED", "ABORTED":
				return fmt.Errorf("job %d %s: %s", id, jobs[0].State, jobs[0].Error)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("job %d did not finish within %s: %w", id, jobTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// SetPerm sets mode and ownership on a dataset's mountpoint.
//
// A freshly created dataset is root:root 0755, so a pod running as a non-root
// user cannot write to it. fsGroup does not fix this — kubelet skips fsGroup
// ownership changes for NFS — so permissions must be set here, at provisioning
// time, on the appliance.
//
// This is the ONLY job-based call the driver makes: it returns an integer job id
// which must be polled through core.get_jobs.
func (c *Ops) SetPerm(ctx context.Context, path, mode string, uid, gid int) error {
	var jobID int
	err := c.CallJSON(ctx, &jobID, "filesystem.setperm", map[string]any{
		"path": path, "mode": mode, "uid": uid, "gid": gid,
		"options": map[string]any{"recursive": true, "traverse": false},
	})
	if err != nil {
		return fmt.Errorf("filesystem.setperm %s: %w", path, err)
	}
	return c.waitForJob(ctx, jobID)
}
