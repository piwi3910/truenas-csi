package csi

import "sync"

// VolumeLocks serialises CSI calls per volume id.
//
// The container orchestrator may issue CreateVolume and DeleteVolume for the
// same volume concurrently and retries everything forever. Without this, two
// racing calls can leave an orphaned zvol or a half-deleted extent behind.
type VolumeLocks struct {
	mu sync.Mutex
	in map[string]struct{}
}

// NewVolumeLocks builds an empty lock set.
func NewVolumeLocks() *VolumeLocks { return &VolumeLocks{in: map[string]struct{}{}} }

// TryAcquire takes the lock for id. The second return is false when another
// call already holds it, and the caller should answer ABORTED so the sidecar
// retries rather than racing.
func (l *VolumeLocks) TryAcquire(id string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.in[id]; busy {
		return nil, false
	}
	l.in[id] = struct{}{}
	return func() {
		l.mu.Lock()
		delete(l.in, id)
		l.mu.Unlock()
	}, true
}
