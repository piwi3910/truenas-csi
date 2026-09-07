package config

import (
	"errors"
	"testing"
)

// TestFlavourValidation pins two things at once: the flavour is a closed set,
// and the transport requirement is flavour-aware. SCALE speaks JSON-RPC over a
// websocket (wss://); CORE speaks REST over HTTPS (https://). Both are
// mandatory for the same reason — a plaintext connection does not merely fail,
// it makes TrueNAS REVOKE the API key.
func TestFlavourValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flavour  string
		endpoint string
		wantErr  error
	}{
		{"empty flavour defaults to scale", "", "wss://nas/api/current", nil},
		{"scale accepts wss", "scale", "wss://nas/api/current", nil},
		{"scale is case-insensitive", "SCALE", "wss://nas/api/current", nil},
		{"scale rejects https", "scale", "https://nas/api/v2.0", ErrInsecureTransport},
		{"scale rejects ws", "scale", "ws://nas/api/current", ErrInsecureTransport},
		{"core accepts https", "core", "https://nas/api/v2.0", nil},
		{"core is case-insensitive", "Core", "HTTPS://nas/api/v2.0", nil},
		{"core rejects http", "core", "http://nas/api/v2.0", ErrInsecureTransport},
		{"core rejects wss", "core", "wss://nas/api/current", ErrInsecureTransport},
		{"core rejects empty endpoint", "core", "", ErrInsecureTransport},
		{"unknown flavour is refused", "truenas-core", "https://nas", ErrUnknownFlavour},
		{"unknown flavour is refused before the endpoint", "enterprise", "wss://nas", ErrUnknownFlavour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := validBackend()
			b.Flavour = tc.flavour
			b.Endpoint = tc.endpoint
			c := &Config{Backends: map[string]Backend{"nas1": b}, NodeID: "n1"}
			err := c.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestFlavourNormalises proves the rest of the driver never has to think about
// case or about an unset value: it always sees "scale" or "core".
func TestFlavourNormalises(t *testing.T) {
	for in, want := range map[string]string{
		"": "scale", "scale": "scale", "SCALE": "scale",
		"core": "core", "Core": "core", " core ": "core",
	} {
		b := validBackend()
		b.Flavour = in
		if got := b.NormalisedFlavour(); got != want {
			t.Errorf("flavour %q: want %q, got %q", in, want, got)
		}
	}
}
