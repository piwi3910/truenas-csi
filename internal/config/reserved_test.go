package config

import (
	"strings"
	"testing"
)

// TestReservedConfigValidation pins the bounds on the pool reservation. A
// percentage outside 0..100 or a negative byte count is an operator typo that
// would otherwise silently produce a nonsensical reserve.
func TestReservedConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bytes   int64
		percent float64
		wantErr string
	}{
		{name: "unset"},
		{name: "bytes only", bytes: 50 << 30},
		{name: "percent only", percent: 10},
		{name: "both", bytes: 50 << 30, percent: 10},
		{name: "percent 0", percent: 0},
		{name: "percent 100", percent: 100},
		{name: "negative bytes", bytes: -1, wantErr: "reservedBytes"},
		{name: "negative percent", percent: -0.5, wantErr: "reservedPercent"},
		{name: "percent over 100", percent: 100.1, wantErr: "reservedPercent"},
	} {
		b := validBackend()
		b.ReservedBytes = tc.bytes
		b.ReservedPercent = tc.percent
		c := &Config{Backends: map[string]Backend{"nas1": b}, NodeID: "n1"}
		err := c.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: want nil, got %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: want an error naming %s, got nil", tc.name, tc.wantErr)
		case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.wantErr)
		}
	}
}

// TestReserveTakesTheLargerReservation proves the two knobs do not add up: when
// both are set the larger wins, so an operator who states both gets the stronger
// guarantee rather than the sum of two independent guesses.
func TestReserveTakesTheLargerReservation(t *testing.T) {
	const poolSize = int64(1000)
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
	} {
		b := validBackend()
		b.ReservedBytes = tc.bytes
		b.ReservedPercent = tc.percent
		if got := b.Reserve(poolSize); got != tc.want {
			t.Errorf("%s: Reserve(%d) = %d, want %d", tc.name, poolSize, got, tc.want)
		}
	}
}
