package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrInsecureTransport is returned for any endpoint that is not wss://.
//
// This is not stylistic. TrueNAS 25.10 REVOKES an API key the moment it is
// presented over a plaintext connection ("API key revoked due to insecure
// transport"), so a misconfigured scheme does not merely fail to connect — it
// destroys the credential and requires an operator to issue a new one by hand.
var ErrInsecureTransport = errors.New(
	"endpoint must use wss:// (scale) or https:// (core) — " +
		"TrueNAS revokes API keys presented over insecure transport")

// ErrUnknownFlavour means a backend named an API dialect this driver has no
// client for. It is a closed set on purpose: a typo must fail at startup rather
// than silently fall back to SCALE and dial a CORE appliance over a websocket
// that does not exist there.
var ErrUnknownFlavour = errors.New(`flavour must be "scale" or "core"`)

// Flavour names an appliance's API dialect.
const (
	FlavourSCALE = "scale"
	FlavourCORE  = "core"
)

// schemeForFlavour is the ONLY scheme each flavour may use. Both are encrypted;
// neither is negotiable, for the same credential-revocation reason.
var schemeForFlavour = map[string]string{
	FlavourSCALE: "wss",
	FlavourCORE:  "https",
}

// NormalisedFlavour is the flavour the rest of the driver sees: lower-cased,
// trimmed, and defaulted to SCALE when unset. An unknown value is returned as
// given so validate() can name it in the error.
func (b Backend) NormalisedFlavour() string {
	f := strings.ToLower(strings.TrimSpace(b.Flavour))
	if f == "" {
		return FlavourSCALE
	}
	return f
}

// Validate checks the configuration before anything opens a connection.
func (c *Config) Validate() error {
	if len(c.Backends) == 0 {
		return errors.New("no backends configured: at least one TrueNAS appliance is required")
	}
	for name, b := range c.Backends {
		if err := b.validate(); err != nil {
			return fmt.Errorf("backend %q: %w", name, err)
		}
	}
	return nil
}

// ValidateNode adds the checks that only the node plugin needs. nodeID names a
// Kubernetes node, so a controller has none and must not be made to invent one.
func (c *Config) ValidateNode() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.NodeID == "" {
		return errors.New("nodeID must be set when running as the node plugin")
	}
	return nil
}

func (b Backend) validate() error {
	flavour := b.NormalisedFlavour()
	scheme, ok := schemeForFlavour[flavour]
	if !ok {
		return fmt.Errorf("flavour %q is not supported: %w", b.Flavour, ErrUnknownFlavour)
	}
	if err := validateEndpoint(b.Endpoint, scheme); err != nil {
		return err
	}
	for _, f := range []struct {
		name, value string
	}{
		{"username", b.Username},
		{"apiKey", b.APIKey},
		{"pool", b.Pool},
		{"parentDataset", b.ParentDataset},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("%s must be set", f.name)
		}
	}
	if strings.Contains(b.ParentDataset, "..") {
		return errors.New("parentDataset must not contain ..")
	}
	if b.ReservedBytes < 0 {
		return fmt.Errorf("reservedBytes must not be negative, got %d", b.ReservedBytes)
	}
	if b.ReservedPercent < 0 || b.ReservedPercent > 100 {
		return fmt.Errorf("reservedPercent must be between 0 and 100, got %v", b.ReservedPercent)
	}
	if b.RateLimit < 0 {
		return fmt.Errorf("rateLimit must not be negative, got %v", b.RateLimit)
	}
	if s := strings.TrimSpace(b.BreakerResetTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return fmt.Errorf("breakerResetTimeout %q is not a positive Go duration (e.g. 10s)",
				b.BreakerResetTimeout)
		}
	}
	return nil
}

func validateEndpoint(endpoint, wantScheme string) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("endpoint must be set: %w", ErrInsecureTransport)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", endpoint, ErrInsecureTransport)
	}
	if !strings.EqualFold(u.Scheme, wantScheme) {
		return fmt.Errorf("endpoint %q uses scheme %q, want %q: %w",
			endpoint, u.Scheme, wantScheme, ErrInsecureTransport)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host: %w", endpoint, ErrInsecureTransport)
	}
	return nil
}
