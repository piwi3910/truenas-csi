package csi

import "testing"

// TestUsableCapacityIsMeasuredInWritableBytes.
//
// pool.query reports RAW bytes: on a RAIDZ pool that includes parity, which can
// never hold data. Measured on the validation appliance, a 12-disk RAIDZ2 pool:
//
//	pool.query free            41.05 TiB   (raw, what the driver used)
//	root dataset available     30.33 TiB   (what can actually be written)
//
// a deflate ratio of 0.739. Reporting the raw figure told the scheduler there
// was 34.50 TiB to place volumes into when 25.35 TiB was the truth after the
// operator's reserve — 36% too high. Provisioning would keep succeeding until
// the pool filled at the real figure and then fail with ENOSPC while
// CSIStorageCapacity still advertised terabytes free.
//
// The same figures drove requireRoomOutsideReserve, so the guard that exists to
// keep headroom was measuring the headroom in raw bytes too.
func TestUsableCapacityIsMeasuredInWritableBytes(t *testing.T) {
	const (
		writableFree int64 = 33352704796320 // root dataset "available"
		writableUsed int64 = 21466962973168 // root dataset "used"
	)
	// A tenth of what the pool can actually hold, not a tenth of its raw size.
	reserve := poolReserve(writableFree, writableUsed, 0, 10)
	if want := (writableFree + writableUsed) / 10; reserve != want {
		t.Fatalf("reserve = %d, want %d (a tenth of the writable total)", reserve, want)
	}

	got := usableCapacity(writableFree, reserve)
	if got >= 30_000_000_000_000 {
		t.Fatalf("usable = %d, which is more than the pool can write", got)
	}
	if want := writableFree - reserve; got != want {
		t.Fatalf("usable = %d, want %d", got, want)
	}

	// An absolute reservation still wins when it is the larger of the two.
	if r := poolReserve(writableFree, writableUsed, 9_000_000_000_000, 10); r != 9_000_000_000_000 {
		t.Fatalf("reserve = %d, want the larger absolute reservation", r)
	}
	// And a pool already inside its reserve reports nothing, never a negative
	// that the provisioner would read as an enormous unsigned figure.
	if got := usableCapacity(100, 500); got != 0 {
		t.Fatalf("usable = %d, want 0 when the pool is inside its reserve", got)
	}
}

// TestPoolReserveTakesTheLargerReservation proves the two knobs do not add up:
// when both are set the larger wins, so an operator who states both gets the
// stronger guarantee rather than the sum of two independent guesses.
//
// This moved here from internal/config with the arithmetic itself, which now
// takes a WRITABLE total rather than pool.query's raw size.
func TestPoolReserveTakesTheLargerReservation(t *testing.T) {
	// A pool that can hold 1000 bytes, 400 of them still free.
	const free, used = int64(400), int64(600)
	for _, tc := range []struct {
		name    string
		bytes   int64
		percent float64
		want    int64
	}{
		{name: "none", want: 0},
		{name: "bytes only", bytes: 250, want: 250},
		{name: "percent only", percent: 10, want: 100},
		{name: "both, bytes larger", bytes: 250, percent: 10, want: 250},
		{name: "both, percent larger", bytes: 50, percent: 30, want: 300},
		{name: "negative is no reservation", bytes: -5, want: 0},
	} {
		if got := poolReserve(free, used, tc.bytes, tc.percent); got != tc.want {
			t.Errorf("%s: poolReserve(%d, %d, %d, %v) = %d, want %d",
				tc.name, free, used, tc.bytes, tc.percent, got, tc.want)
		}
	}
}
