// Package health reads the driver's own metrics to find out what it thinks of
// its appliances.
//
// The operator deliberately does not open its own websocket to TrueNAS. Doing
// so would mean a second process holding the same API key, a second connection
// counting against the appliance's concurrency budget, and — because TrueNAS
// revokes a key presented over a bad transport — a second chance to destroy the
// credential. The driver already knows whether each backend is up and how many
// orphaned datasets it last found; the operator asks the driver.
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Backend is what the driver reports about one appliance.
type Backend struct {
	// Up is the truenas_csi_backend_up gauge.
	Up bool
	// Orphans is the truenas_csi_orphaned_volumes gauge: datasets this driver
	// owns with no matching PersistentVolume. The driver never deletes them;
	// the number is a prompt to investigate, not a failure.
	Orphans int32
}

const (
	metricBackendUp = "truenas_csi_backend_up"
	metricOrphans   = "truenas_csi_orphaned_volumes"
)

// Parse reads a Prometheus text exposition and returns per-backend health.
func Parse(r io.Reader) (map[string]Backend, error) {
	// The parser needs an explicit name validation scheme; the zero value panics.
	// Legacy validation is right here: the driver's metric names are all plain
	// ASCII, and accepting UTF-8 names would only widen what a compromised
	// metrics endpoint could feed into this process.
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("parse driver metrics: %w", err)
	}
	out := map[string]Backend{}
	for _, name := range []string{metricBackendUp, metricOrphans} {
		fam, ok := families[name]
		if !ok {
			continue
		}
		for _, m := range fam.GetMetric() {
			backend := ""
			for _, l := range m.GetLabel() {
				if l.GetName() == "backend" {
					backend = l.GetValue()
				}
			}
			if backend == "" {
				continue
			}
			value := m.GetGauge().GetValue()
			entry := out[backend]
			switch name {
			case metricBackendUp:
				entry.Up = value >= 1
			case metricOrphans:
				entry.Orphans = int32(value)
			}
			out[backend] = entry
		}
	}
	return out, nil
}

// Scraper fetches the driver's metrics endpoint.
type Scraper struct {
	Client *http.Client
}

// NewScraper builds a Scraper with a short timeout: a hung metrics endpoint
// must not stall reconciliation of the whole driver.
func NewScraper() *Scraper {
	return &Scraper{Client: &http.Client{Timeout: 5 * time.Second}}
}

// Scrape reads per-backend health from one driver metrics URL.
func (s *Scraper) Scrape(ctx context.Context, url string) (map[string]Backend, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: status %d", url, resp.StatusCode)
	}
	return Parse(io.LimitReader(resp.Body, 4<<20))
}
