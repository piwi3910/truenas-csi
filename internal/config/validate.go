package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrInsecureTransport is returned for any endpoint that is not wss://.
//
// This is not stylistic. TrueNAS 25.10 REVOKES an API key the moment it is
// presented over a plaintext connection ("API key revoked due to insecure
// transport"), so a misconfigured scheme does not merely fail to connect — it
// destroys the credential and requires an operator to issue a new one by hand.
var ErrInsecureTransport = errors.New(
	"endpoint must use wss:// — TrueNAS revokes API keys presented over insecure transport")

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
	if err := validateEndpoint(b.Endpoint); err != nil {
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
	return nil
}

func validateEndpoint(endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("endpoint must be set: %w", ErrInsecureTransport)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", endpoint, ErrInsecureTransport)
	}
	if !strings.EqualFold(u.Scheme, "wss") {
		return fmt.Errorf("endpoint %q uses scheme %q: %w", endpoint, u.Scheme, ErrInsecureTransport)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host: %w", endpoint, ErrInsecureTransport)
	}
	return nil
}
