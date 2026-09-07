package driver

import "testing"

func TestDriverName(t *testing.T) {
	if DriverName != "csi.truenas.watteel.com" {
		t.Fatalf("got %q", DriverName)
	}
}
