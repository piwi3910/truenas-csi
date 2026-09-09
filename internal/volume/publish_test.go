package volume

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestGrantsRoundTrip: the ledger is stored as a ZFS user property and read
// back by a controller that may never have written it, so encoding and decoding
// have to agree exactly.
func TestGrantsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Grants
	}{
		{"empty", Grants{}},
		{"one node", Grants{"worker-1": {"10.0.0.1"}}},
		{"several addresses", Grants{"worker-1": {"10.0.0.1", "192.168.1.1"}}},
		{"several nodes", Grants{"worker-1": {"10.0.0.1"}, "worker-2": {"10.0.0.2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := tc.in.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := DecodeGrants(encoded)
			if err != nil {
				t.Fatalf("DecodeGrants(%q): %v", encoded, err)
			}
			if len(got) != len(tc.in) {
				t.Fatalf("round trip lost entries: %v -> %q -> %v", tc.in, encoded, got)
			}
			for node, addrs := range tc.in {
				if !slices.Equal(got[node], addrs) {
					t.Errorf("%s: got %v, want %v", node, got[node], addrs)
				}
			}
		})
	}
}

// TestDecodeGrantsTreatsAnAbsentLedgerAsEmpty: every volume provisioned before
// this driver kept a ledger has no property at all, and the first publish after
// an upgrade must simply start the record rather than fail.
func TestDecodeGrantsTreatsAnAbsentLedgerAsEmpty(t *testing.T) {
	g, err := DecodeGrants("")
	if err != nil {
		t.Fatalf("an absent ledger must not be an error, got %v", err)
	}
	if len(g) != 0 {
		t.Fatalf("want an empty ledger, got %v", g)
	}
	if _, err := DecodeGrants("not json"); err == nil {
		t.Fatal("a corrupt ledger must be reported, not silently treated as empty")
	}
}

// TestGrantsOthers is what makes a single-node access mode enforceable: the
// appliance cannot say which node holds a shared target's LUN, so the second
// publisher is recognised here or nowhere.
func TestGrantsOthers(t *testing.T) {
	g := Grants{"worker-1": {"10.0.0.1"}, "worker-2": {"10.0.0.2"}}
	if got := g.Others("worker-1"); !slices.Equal(got, []string{"worker-2"}) {
		t.Errorf("Others(worker-1) = %v, want [worker-2]", got)
	}
	if got := g.Others("worker-3"); !slices.Equal(got, []string{"worker-1", "worker-2"}) {
		t.Errorf("Others(worker-3) = %v, want both nodes", got)
	}
	empty := Grants{}
	if got := empty.Others("worker-1"); len(got) != 0 {
		t.Errorf("an empty ledger holds nobody, got %v", got)
	}
	if got := g.Nodes(); !slices.Equal(got, []string{"worker-1", "worker-2"}) {
		t.Errorf("Nodes() must be stable and sorted, got %v", got)
	}
}

// TestGrantsRefuseToOutgrowAProperty: a ZFS user property caps at 8 KiB, and a
// ledger that silently truncated would leave granted access nothing could ever
// revoke.
func TestGrantsRefuseToOutgrowAProperty(t *testing.T) {
	g := Grants{}
	for i := range 500 {
		g[strings.Repeat("n", 20)+string(rune('a'+i%26))+string(rune('a'+i/26))] =
			[]string{"10.0.0.1", "10.0.0.2", "192.168.100.100"}
	}
	if _, err := g.Encode(); !errors.Is(err, ErrGrantsTooLarge) {
		t.Fatalf("want ErrGrantsTooLarge, got %v", err)
	}
}

// TestGrantsRefuseAtTheMiddlewareLimit pins the ledger bound to what the
// appliance will actually store.
//
// It was 4096, taken from ZFS's own 8 KiB user-property limit. The middleware
// is far stricter: pool.dataset.update refuses any value over 1024 characters
// with a Pydantic schema error. Measured on 25.10.6 -- exactly 1024 accepted,
// 1025 refused. So a ledger between 1025 and 4096 bytes passed this check and
// was then rejected by the appliance, and ErrGrantsTooLarge, which exists to
// explain precisely this, never fired.
func TestGrantsRefuseAtTheMiddlewareLimit(t *testing.T) {
	if maxGrantsBytes != 1024 {
		t.Fatalf("maxGrantsBytes = %d, want 1024: the middleware refuses a longer "+
			"user property value, whatever ZFS itself allows", maxGrantsBytes)
	}

	// Grow a realistic dual-stack ledger until Encode refuses, and check that
	// everything it DID accept would really have fitted.
	g := Grants{}
	for i := 1; i <= 200; i++ {
		g[fmt.Sprintf("worker-%d", i)] = []string{
			fmt.Sprintf("192.168.10.%d", 100+i),
			fmt.Sprintf("fd7c:8f2a:1b3c:4d5e::%x", 0x1000+i),
		}
		encoded, err := g.Encode()
		if err != nil {
			if !errors.Is(err, ErrGrantsTooLarge) {
				t.Fatalf("Encode failed with the wrong error at %d nodes: %v", i, err)
			}
			if i < 10 {
				t.Fatalf("the ledger was refused at only %d nodes, which would break "+
					"ordinary ReadWriteMany use", i)
			}
			return
		}
		if len(encoded) > maxGrantsBytes {
			t.Fatalf("Encode accepted %d bytes at %d nodes, which the appliance "+
				"would refuse", len(encoded), i)
		}
	}
	t.Fatal("the ledger never hit its bound, so the guard cannot be firing")
}
