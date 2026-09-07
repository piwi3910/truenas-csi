package integration

// Every protocol backend registers itself from init(), so a test binary that
// does not import one simply does not have that protocol — it fails with
// "protocol is not supported by this driver version" rather than anything that
// points at the real cause. Import them all here, in one place, so adding a
// backend does not silently skip its end-to-end coverage.
import (
	_ "github.com/piwi3910/truenas-csi/internal/backend/iscsi"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nfs"
	_ "github.com/piwi3910/truenas-csi/internal/backend/smb"
)
