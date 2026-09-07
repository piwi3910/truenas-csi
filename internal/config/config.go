// Package config loads and validates the driver's configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Backend is one TrueNAS appliance the driver provisions against.
type Backend struct {
	Name          string `yaml:"name"`
	Endpoint      string `yaml:"endpoint"`
	Username      string `yaml:"username"`
	APIKey        string `yaml:"apiKey"`
	Pool          string `yaml:"pool"`
	ParentDataset string `yaml:"parentDataset"`

	// CACert, when set, is the only certificate trusted for this appliance.
	CACert []byte `yaml:"caCert"`
	// InsecureSkipVerify disables certificate verification. A stock TrueNAS
	// certificate is self-signed with SAN=DNS:localhost and cannot be verified
	// against a real address, so this exists — but it is never the default.
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

// String renders the backend without its credentials, so it is safe to log.
func (b Backend) String() string {
	key := "[redacted]"
	if b.APIKey == "" {
		key = "[unset]"
	}
	return fmt.Sprintf("Backend{Name:%s Endpoint:%s Username:%s APIKey:%s Pool:%s ParentDataset:%s InsecureSkipVerify:%t}",
		b.Name, b.Endpoint, b.Username, key, b.Pool, b.ParentDataset, b.InsecureSkipVerify)
}

// Config is the whole driver configuration.
type Config struct {
	Backends    map[string]Backend `yaml:"backends"`
	NodeID      string             `yaml:"nodeID"`
	LogLevel    string             `yaml:"logLevel"`
	MetricsAddr string             `yaml:"metricsAddr"`
	HealthAddr  string             `yaml:"healthAddr"`
}

// Load reads and validates a YAML configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	for name, b := range c.Backends {
		if b.Name == "" {
			b.Name = name
			c.Backends[name] = b
		}
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9090"
	}
	if c.HealthAddr == "" {
		c.HealthAddr = ":9808"
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
