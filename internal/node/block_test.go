package node

import (
	"context"
	"strings"
	"testing"
)

// TestPublishBlockResolvesByProtocol pins the fix for a raw-block NVMe volume
// that could never be published.
//
// publishBlock demanded a NAA unconditionally, which only iSCSI has: an NVMe
// namespace is identified by its subsystem serial. So every volumeMode: Block
// NVMe volume failed NodePublishVolume with `block volume needs "naa" in the
// publish context`, the kubelet retried for ever, and the pod sat in
// ContainerCreating -- while the same volume as a filesystem worked, which is
// why it survived every hand-run test. The upstream conformance suite's block
// patterns found it.
//
// What is asserted here is the DISPATCH: each protocol is asked for its own
// identifier. Resolving the device itself needs a real host and is covered on
// hardware.
func TestPublishBlockResolvesByProtocol(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pc      map[string]string
		wantErr string
	}{
		{
			name:    "nvme without a serial says so",
			pc:      map[string]string{KeyProtocol: ProtocolNVMe},
			wantErr: KeySerial,
		},
		{
			name:    "iscsi still needs its NAA",
			pc:      map[string]string{KeyProtocol: ProtocolISCSI},
			wantErr: KeyNAA,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &Node{}
			_, err := n.resolveBlockDevice(context.Background(), tc.pc, protocolOf(tc.pc))
			if err == nil {
				t.Fatal("expected the missing identifier to be reported")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if tc.pc[KeyProtocol] == ProtocolNVMe && strings.Contains(err.Error(), KeyNAA) {
				t.Errorf("an NVMe volume was refused for want of a NAA: %v", err)
			}
		})
	}
}
