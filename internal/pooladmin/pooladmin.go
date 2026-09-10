// Package pooladmin exposes the appliance's own maintenance surface — pool
// health, scrub state, disk health and active alerts — as READ-ONLY diagnostics
// an operator can consult without opening the TrueNAS UI.
//
// The package is strictly read-only and must stay that way. It never starts a
// scrub, never replaces a disk, never dismisses an alert and never touches a
// dataset. Managing the pool is the appliance's job; this driver only reports
// what the appliance says, so that a Kubernetes-side bug can never turn into a
// storage-side action on somebody's array.
//
// That rule is enforced twice: every call goes through query(), which refuses a
// method that is not a recognised read verb, and TestPoolAdminIsReadOnly drives
// every exported function and fails if the appliance saw anything else.
package pooladmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// ErrMutatingCall means this package was asked to issue a method that is not a
// read. It is a programming error, caught before the call reaches the wire.
var ErrMutatingCall = errors.New("pooladmin is read-only and refuses to call this method")

// Backend is one appliance to report on.
type Backend struct {
	// Name is the configured backend name, used as the metric label.
	Name string
	// Client is the connected middleware client.
	Client *truenas.Client
}

// readVerbs is the closed set of trailing method components this package may
// call. Everything else — run, start, update, delete, replace, dismiss — is a
// mutation and is refused.
var readVerbs = map[string]bool{
	"query":        true,
	"list":         true,
	"config":       true,
	"get_instance": true,
	"results":      true,
	"temperatures": true,
	"get_disks":    true,
}

// query issues one read-only middleware call.
//
// The verb check is the point: it is the single funnel every call in this
// package goes through, so a mutating method cannot be introduced by editing
// one diagnostic in isolation.
func query(ctx context.Context, b Backend, out any, method string, params ...any) error {
	verb := method
	if i := strings.LastIndex(method, "."); i >= 0 {
		verb = method[i+1:]
	}
	if !readVerbs[verb] {
		return fmt.Errorf("%w: %q", ErrMutatingCall, method)
	}
	if b.Client == nil {
		return fmt.Errorf("backend %q has no client", b.Name)
	}
	start := time.Now()
	err := b.Client.CallJSON(ctx, out, method, params...)
	obs.ObserveMiddleware(method, err, time.Since(start))
	return err
}

// Scrub is the state of a pool's scrub as the appliance last reported it.
type Scrub struct {
	Function        string
	State           string
	Errors          int64
	PercentComplete float64
	Start           time.Time
	End             time.Time
}

// Pool is one pool's health, capacity and scrub state.
type Pool struct {
	Name                 string
	Status               string
	Healthy              bool
	SizeBytes            int64
	FreeBytes            int64
	UsedPercent          float64
	FragmentationPercent float64
	Scrub                Scrub
}

// Disk is one disk's state and SMART summary, where the appliance exposes one.
type Disk struct {
	Name      string
	Serial    string
	Model     string
	Pool      string
	SizeBytes int64
	// SMARTAvailable reports whether the appliance exposes SMART at all.
	//
	// TrueNAS 25.10 has NO smart.* methods: the namespace was removed from the
	// middleware, and disk.query no longer returns a "togglesmart" field. The
	// driver asked for both and got nothing, then rendered the nothing as
	// "SMART: disabled" for every disk on every modern appliance -- a claim
	// about the operator's hardware that was never checked. Absent is not
	// disabled, and this field is what keeps the two apart.
	SMARTAvailable bool
	// SMARTStatus is the appliance's own word for the last SMART test result
	// ("SUCCESS", "FAILED", "RUNNING"), or "UNKNOWN" when it exposes none.
	SMARTStatus string
	// LastTest describes the last SMART test, when one is reported.
	LastTest string
	// Healthy is false only when the appliance says something is wrong. An
	// absent SMART result reads as healthy-but-unknown rather than failed, so a
	// controller without SMART passthrough does not page an operator nightly.
	Healthy bool
	// AlertedBy names the appliance alert classes that mention this disk's
	// serial, empty when none do.
	//
	// It exists because 25.10 exposes no SMART surface, so the only thing the
	// appliance will tell anyone about a failing disk is an alert. Reporting
	// SMART as "unavailable" and the disk as healthy, while the same appliance
	// is raising SMARTUncorrectedErrors against it, is two true statements
	// adding up to a false impression.
	AlertedBy string
}

// markAlertedDisks flags every disk whose SERIAL appears in an active alert.
//
// Serial, not device name: sd* names are assigned by the kernel and move
// between boots, so joining on them would follow the wrong disk after a
// reboot, while a serial is unique and cannot collide with unrelated text.
//
// A disk the appliance reports without a serial is left alone rather than
// matched against everything, which is what an empty needle would do.
func markAlertedDisks(disks []Disk, alerts []Alert) []Disk {
	out := make([]Disk, len(disks))
	copy(out, disks)
	for i := range out {
		if out[i].Serial == "" {
			continue
		}
		var classes []string
		for _, a := range alerts {
			if a.Dismissed || !strings.Contains(a.Formatted, out[i].Serial) {
				continue
			}
			if !slices.Contains(classes, a.Class) {
				classes = append(classes, a.Class)
			}
		}
		if len(classes) == 0 {
			continue
		}
		sort.Strings(classes)
		out[i].AlertedBy = strings.Join(classes, ",")
		out[i].Healthy = false
	}
	return out
}

// Alert is one active appliance alert.
type Alert struct {
	ID        string
	Level     string
	Class     string
	Formatted string
	Dismissed bool
	Time      time.Time
}

// Diagnostics is everything this package can report about one appliance.
type Diagnostics struct {
	Backend string
	Pools   []Pool
	Disks   []Disk
	Alerts  []Alert
	// Errors holds per-section failures. A missing SMART surface must not cost
	// an operator the pool status they asked for, so the sections are collected
	// independently and their failures reported rather than returned.
	Errors []string
}

// PoolStatus reports pool health, scrub state, capacity and fragmentation.
func PoolStatus(ctx context.Context, b Backend) ([]Pool, error) {
	var raw []struct {
		Name          string          `json:"name"`
		Status        string          `json:"status"`
		Healthy       bool            `json:"healthy"`
		Size          json.RawMessage `json:"size"`
		Free          json.RawMessage `json:"free"`
		Fragmentation json.RawMessage `json:"fragmentation"`
		Scan          *struct {
			Function   string          `json:"function"`
			State      string          `json:"state"`
			Errors     int64           `json:"errors"`
			Percentage float64         `json:"percentage"`
			StartTime  json.RawMessage `json:"start_time"`
			EndTime    json.RawMessage `json:"end_time"`
		} `json:"scan"`
	}
	if err := query(ctx, b, &raw, "pool.query"); err != nil {
		return nil, fmt.Errorf("backend %q: pool.query: %w", b.Name, err)
	}

	out := make([]Pool, 0, len(raw))
	for _, r := range raw {
		p := Pool{
			Name:                 r.Name,
			Status:               r.Status,
			Healthy:              r.Healthy,
			SizeBytes:            number(r.Size),
			FreeBytes:            number(r.Free),
			FragmentationPercent: float64(number(r.Fragmentation)),
		}
		if p.SizeBytes > 0 {
			p.UsedPercent = float64(p.SizeBytes-p.FreeBytes) / float64(p.SizeBytes) * 100
		}
		if r.Scan != nil {
			p.Scrub = Scrub{
				Function:        r.Scan.Function,
				State:           r.Scan.State,
				Errors:          r.Scan.Errors,
				PercentComplete: r.Scan.Percentage,
				Start:           timestamp(r.Scan.StartTime),
				End:             timestamp(r.Scan.EndTime),
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// DiskHealth reports per-disk state, with the SMART summary where the appliance
// exposes one and any active alert that names the disk.
//
// The alert join is part of DiskHealth rather than of one caller because it is
// what every caller needs: on 25.10 there is no SMART surface at all, so an
// alert is the ONLY thing the appliance will say about a failing disk. Putting
// it in Collect alone left `pool disks` — the command an operator actually runs
// to look at disks — reporting a disk with uncorrectable errors as though
// nothing were known about it.
//
// A failure to read the alerts costs the alert column, not the inventory.
func DiskHealth(ctx context.Context, b Backend) ([]Disk, error) {
	disks, err := diskInventory(ctx, b)
	if err != nil {
		return nil, err
	}
	alerts, err := Alerts(ctx, b)
	if err != nil {
		return disks, nil
	}
	return markAlertedDisks(disks, alerts), nil
}

// diskInventory is the disk listing itself, without the alert join.
//
// A missing smart.test.results is not an error: TrueNAS CORE, a controller
// without SMART passthrough and a pool of NVMe namespaces all legitimately
// report nothing there, and losing the disk inventory over it would be worse
// than reporting the SMART status as unknown.
func diskInventory(ctx context.Context, b Backend) ([]Disk, error) {
	var raw []struct {
		Name   string          `json:"name"`
		Serial string          `json:"serial"`
		Model  string          `json:"model"`
		Pool   string          `json:"pool"`
		Size   json.RawMessage `json:"size"`
	}
	// extra.pools is not optional decoration: without it disk.query returns
	// "pool": null for EVERY disk, and the report showed a blank pool column on
	// an appliance whose disks were all in one. Verified on 25.10.6.
	if err := query(ctx, b, &raw, "disk.query",
		[]any{}, map[string]any{"extra": map[string]any{"pools": true}}); err != nil {
		return nil, fmt.Errorf("backend %q: disk.query: %w", b.Name, err)
	}

	smart, smartAvailable := smartResults(ctx, b)

	out := make([]Disk, 0, len(raw))
	for _, r := range raw {
		d := Disk{
			Name:           r.Name,
			Serial:         r.Serial,
			Model:          r.Model,
			Pool:           r.Pool,
			SizeBytes:      number(r.Size),
			SMARTAvailable: smartAvailable,
			SMARTStatus:    "UNKNOWN",
			Healthy:        true,
		}
		if s, ok := smart[r.Name]; ok {
			d.SMARTStatus = s.status
			d.LastTest = s.description
			// Only an explicit failure makes a disk unhealthy. "RUNNING" and an
			// absent result are both "nothing is known to be wrong".
			d.Healthy = s.status != "FAILED"
		}
		out = append(out, d)
	}
	return out, nil
}

type smartSummary struct {
	status      string
	description string
}

// smartResults reads the last SMART test per disk, tolerating an appliance that
// does not expose the method at all.
func smartResults(ctx context.Context, b Backend) (map[string]smartSummary, bool) {
	var raw []struct {
		Disk  string `json:"disk"`
		Tests []struct {
			Status      string `json:"status"`
			Description string `json:"description"`
			Lifetime    int64  `json:"lifetime"`
		} `json:"tests"`
	}
	if err := query(ctx, b, &raw, "smart.test.results"); err != nil {
		// 25.10 removed the whole smart.* namespace, so this is the normal
		// answer on a current appliance rather than a fault. Reporting it as
		// "unavailable" is the point: the previous code turned this failure
		// into "SMART: disabled" on every disk.
		obs.Logger(ctx).Debug("the appliance exposes no SMART API; disks are reported without one",
			"backend", b.Name, "error", err.Error())
		return nil, false
	}
	out := map[string]smartSummary{}
	for _, r := range raw {
		if len(r.Tests) == 0 {
			continue
		}
		last := r.Tests[0]
		for _, t := range r.Tests {
			if t.Lifetime >= last.Lifetime {
				last = t
			}
		}
		out[r.Disk] = smartSummary{status: last.Status, description: last.Description}
	}
	return out, true
}

// Alerts reports the appliance's active alerts.
func Alerts(ctx context.Context, b Backend) ([]Alert, error) {
	var raw []struct {
		ID        string          `json:"id"`
		Level     string          `json:"level"`
		Klass     string          `json:"klass"`
		Formatted string          `json:"formatted"`
		Dismissed bool            `json:"dismissed"`
		Datetime  json.RawMessage `json:"datetime"`
	}
	if err := query(ctx, b, &raw, "alert.list"); err != nil {
		return nil, fmt.Errorf("backend %q: alert.list: %w", b.Name, err)
	}
	out := make([]Alert, 0, len(raw))
	for _, r := range raw {
		out = append(out, Alert{
			ID:        r.ID,
			Level:     strings.ToUpper(r.Level),
			Class:     r.Klass,
			Formatted: r.Formatted,
			Dismissed: r.Dismissed,
			Time:      timestamp(r.Datetime),
		})
	}
	return out, nil
}

// Collect gathers every diagnostic and publishes it to the metrics registry.
//
// A section that fails is recorded in Errors rather than returned, so one
// unavailable surface does not hide the two that worked. Collect returns an
// error only when nothing at all could be read.
func Collect(ctx context.Context, b Backend) (*Diagnostics, error) {
	d := &Diagnostics{Backend: b.Name}

	pools, err := PoolStatus(ctx, b)
	if err != nil {
		d.Errors = append(d.Errors, err.Error())
	} else {
		d.Pools = pools
		for _, p := range pools {
			obs.SetPoolCapacity(b.Name, p.Name, p.Healthy, p.SizeBytes, p.FreeBytes, p.FragmentationPercent)
			state := p.Scrub.State
			if state == "" {
				state = "NONE"
			}
			obs.SetScrubState(b.Name, p.Name, state, p.Scrub.Errors)
		}
	}

	disks, diskErr := diskInventory(ctx, b)
	if diskErr != nil {
		d.Errors = append(d.Errors, diskErr.Error())
	}

	alerts, err := Alerts(ctx, b)
	if err != nil {
		d.Errors = append(d.Errors, err.Error())
	} else {
		d.Alerts = alerts
		byLevel := map[string]int{}
		for _, a := range alerts {
			byLevel[a.Level]++
		}
		obs.SetApplianceAlerts(b.Name, byLevel)
	}

	// Alerts are read BEFORE the disks are published, because on an appliance
	// with no SMART surface they are the only thing that will say a disk is
	// failing. Publishing the disks first would export truenas_disk_healthy=1
	// for a disk this same call already knows the appliance is alerting on.
	if diskErr == nil {
		d.Disks = markAlertedDisks(disks, d.Alerts)
		for _, dk := range d.Disks {
			obs.SetDiskHealthy(b.Name, dk.Name, dk.Healthy)
		}
	}

	if len(d.Errors) == 3 {
		return d, fmt.Errorf("backend %q: no diagnostics could be read: %s",
			b.Name, strings.Join(d.Errors, "; "))
	}
	return d, nil
}

// number decodes a byte or percentage field, which the middleware reports
// variously as a JSON number, a decimal string, or the {"parsed": N} wrapper.
func number(b json.RawMessage) int64 {
	if len(b) == 0 {
		return 0
	}
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		return n
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		return int64(f)
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(v)
		}
		return 0
	}
	var wrapper struct {
		Parsed json.RawMessage `json:"parsed"`
	}
	if err := json.Unmarshal(b, &wrapper); err == nil && len(wrapper.Parsed) > 0 {
		return number(wrapper.Parsed)
	}
	return 0
}

// timestamp decodes the {"$date": <epoch millis>} shape the middleware uses,
// tolerating a plain number or an RFC 3339 string.
func timestamp(b json.RawMessage) time.Time {
	if len(b) == 0 {
		return time.Time{}
	}
	var wrapper struct {
		Date *int64 `json:"$date"`
	}
	if err := json.Unmarshal(b, &wrapper); err == nil && wrapper.Date != nil {
		return time.UnixMilli(*wrapper.Date).UTC()
	}
	var ms int64
	if err := json.Unmarshal(b, &ms); err == nil && ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
