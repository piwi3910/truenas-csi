// Package config loads and validates the driver's configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

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

	// ReservedBytes is an absolute amount of pool space this driver must never
	// hand out. TrueNAS needs headroom for its system datasets, for snapshots
	// that already exist on the pool and for replication targets; without a
	// reservation, PVCs can fill the pool to the last byte and degrade the
	// appliance itself.
	ReservedBytes int64 `yaml:"reservedBytes"`
	// ReservedPercent reserves a share of the pool's total size, 0..100. It
	// scales with the pool where ReservedBytes does not.
	//
	// When both are set the LARGER of the two reservations wins — they are two
	// ways of stating the same headroom, not two headrooms to be added up.
	ReservedPercent float64 `yaml:"reservedPercent"`
	// Flavour selects which TrueNAS API this appliance speaks: "scale"
	// (default) for the JSON-RPC websocket middleware, or "core" for the
	// legacy REST v2 API. It also decides which transport is demanded of the
	// endpoint — wss:// for SCALE, https:// for CORE.
	//
	// CORE support is implemented against the documented REST shapes and
	// exercised only against a recorded fake; it is UNVERIFIED on real CORE
	// hardware. See README.md and docs/troubleshooting.md.
	Flavour string `yaml:"flavour"`

	// RateLimit caps how many middleware calls per second this driver makes to
	// the appliance, 0 for the client's default. It is a ceiling on a runaway
	// caller, not a throughput target: see truenas.DefaultRateLimit.
	RateLimit float64 `yaml:"rateLimit"`
	// BreakerThreshold is how many consecutive transport-level failures open
	// the circuit breaker, 0 for the client's default and a negative value to
	// disable the breaker entirely.
	BreakerThreshold int `yaml:"breakerThreshold"`
	// BreakerResetTimeout is how long the breaker stays open before it lets a
	// single probe through, as a Go duration ("10s"). Empty means the client's
	// default.
	BreakerResetTimeout string `yaml:"breakerResetTimeout"`

	// NamespaceQuotas is optional per-Kubernetes-namespace capacity accounting.
	// It is off by default and changes the dataset layout of every volume
	// created after it is switched on; see NamespaceQuotas.
	NamespaceQuotas NamespaceQuotas `yaml:"namespaceQuotas"`

	// CACert, when set, is the only certificate trusted for this appliance.
	CACert []byte `yaml:"caCert"`
	// InsecureSkipVerify disables certificate verification. A stock TrueNAS
	// certificate is self-signed with SAN=DNS:localhost and cannot be verified
	// against a real address, so this exists — but it is never the default.
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

// Reserve returns the number of bytes of poolSize this driver must leave
// untouched: the larger of ReservedBytes and ReservedPercent of poolSize, and
// zero when neither is configured. The result is never negative.
func (b Backend) Reserve(poolSize int64) int64 {
	reserve := b.ReservedBytes
	if b.ReservedPercent > 0 && poolSize > 0 {
		if pct := int64(float64(poolSize) * b.ReservedPercent / 100); pct > reserve {
			reserve = pct
		}
	}
	if reserve < 0 {
		return 0
	}
	return reserve
}

// BreakerReset is the configured circuit-breaker reset timeout, or zero when
// unset. Load has already rejected an unparsable value, so the zero here only
// covers a Backend built in code; the client substitutes its own default.
func (b Backend) BreakerReset() time.Duration {
	if strings.TrimSpace(b.BreakerResetTimeout) == "" {
		return 0
	}
	d, err := time.ParseDuration(b.BreakerResetTimeout)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// String renders the backend without its credentials, so it is safe to log.
func (b Backend) String() string {
	key := "[redacted]"
	if b.APIKey == "" {
		key = "[unset]"
	}
	return fmt.Sprintf("Backend{Name:%s Flavour:%s Endpoint:%s Username:%s APIKey:%s Pool:%s ParentDataset:%s InsecureSkipVerify:%t}",
		b.Name, b.NormalisedFlavour(), b.Endpoint, b.Username, key, b.Pool, b.ParentDataset, b.InsecureSkipVerify)
}

// Config is the whole driver configuration.
type Config struct {
	Backends    map[string]Backend `yaml:"backends"`
	NodeID      string             `yaml:"nodeID"`
	LogLevel    string             `yaml:"logLevel"`
	MetricsAddr string             `yaml:"metricsAddr"`
	HealthAddr  string             `yaml:"healthAddr"`

	// MetricsInterval is how often the controller polls each appliance for
	// array-level metrics, as a Go duration ("60s", "5m"). Empty means the
	// package default. Polling is deliberately decoupled from scraping, so
	// this only bounds staleness, never scrape latency.
	MetricsInterval string `yaml:"metricsInterval"`
}

// DefaultMetricsInterval is used when metricsInterval is unset.
const DefaultMetricsInterval = 60 * time.Second

// MetricsPollInterval is the configured array-metrics poll interval.
// Load has already rejected an unparsable value, so the default here only
// covers a Config built in code.
func (c *Config) MetricsPollInterval() time.Duration {
	if c.MetricsInterval == "" {
		return DefaultMetricsInterval
	}
	d, err := time.ParseDuration(c.MetricsInterval)
	if err != nil || d <= 0 {
		return DefaultMetricsInterval
	}
	return d
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
	if c.MetricsInterval != "" {
		d, dErr := time.ParseDuration(c.MetricsInterval)
		if dErr != nil || d <= 0 {
			return nil, fmt.Errorf("metricsInterval %q is not a positive Go duration (e.g. 60s, 5m)",
				c.MetricsInterval)
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
