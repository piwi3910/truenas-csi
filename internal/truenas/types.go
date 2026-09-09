package truenas

import (
	"encoding/json"
	"strconv"
	"time"
)

// Property is one ZFS property as the middleware reports it.
//
// Source matters as much as Value: ZFS user properties are INHERITED by child
// datasets, so a property that is present but INHERITED is not evidence that
// this driver created the dataset. Ownership checks require Source == "LOCAL".
type Property struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// sizeField decodes the {"parsed": N, "rawvalue": "N", ...} shape the middleware
// uses for byte quantities, tolerating numbers, strings and null.
type sizeField struct{ Parsed int64 }

func (s *sizeField) UnmarshalJSON(b []byte) error {
	// Not every endpoint uses the wrapper. pool.query returns plain integers
	// for size and free, while pool.dataset.query wraps them in {"parsed": N}.
	// Decoding only the wrapper silently yielded 0 free bytes for every pool,
	// which would make the scheduler treat a healthy pool as full.
	var direct int64
	if err := json.Unmarshal(b, &direct); err == nil {
		s.Parsed = direct
		return nil
	}
	var directStr string
	if err := json.Unmarshal(b, &directStr); err == nil {
		if v, err := strconv.ParseInt(directStr, 10, 64); err == nil {
			s.Parsed = v
		}
		return nil
	}
	var wrapper struct {
		Parsed   json.RawMessage `json:"parsed"`
		RawValue string          `json:"rawvalue"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return nil // an unexpected shape means "unset"
	}
	var n int64
	if err := json.Unmarshal(wrapper.Parsed, &n); err == nil {
		s.Parsed = n
		return nil
	}
	var str string
	if err := json.Unmarshal(wrapper.Parsed, &str); err == nil {
		if v, err := strconv.ParseInt(str, 10, 64); err == nil {
			s.Parsed = v
			return nil
		}
	}
	// "parsed" is not always a number or a numeric string. A snapshot's
	// creation time parses as {"$date": <milliseconds>}, and an unset property
	// parses as null. "rawvalue" is the machine form in every case the driver
	// reads — unix SECONDS for creation, a byte count for a size — so it is the
	// fallback rather than a second shape to special-case.
	if v, err := strconv.ParseInt(wrapper.RawValue, 10, 64); err == nil {
		s.Parsed = v
	}
	return nil
}

type propField struct{ Property }

func (p *propField) UnmarshalJSON(b []byte) error {
	var raw struct {
		Value  json.RawMessage `json:"value"`
		Source string          `json:"source"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	p.Source = raw.Source
	var s string
	if err := json.Unmarshal(raw.Value, &s); err == nil {
		p.Value = s
	}
	return nil
}

// Dataset is a ZFS filesystem or volume.
type Dataset struct {
	ID         string `json:"id"`
	Type       string `json:"type"` // FILESYSTEM or VOLUME
	Mountpoint string `json:"mountpoint"`

	VolSize   sizeField `json:"volsize"`
	RefQuota  sizeField `json:"refquota"`
	Used      sizeField `json:"used"`
	Available sizeField `json:"available"`

	Origin         propField            `json:"origin"`
	UserProperties map[string]propField `json:"user_properties"`

	// The native ZFS properties the driver is allowed to change on a live
	// volume. They are decoded — rather than read back from a second call —
	// because ControllerModifyVolume has to be idempotent: the only way to
	// answer "is this already the value?" without writing is to compare
	// against what pool.dataset.query already returned.
	Sync        propField `json:"sync"`
	Compression propField `json:"compression"`
	ATime       propField `json:"atime"`
	RecordSize  propField `json:"recordsize"`
}

// ZFSProperty returns one of the native ZFS properties this client decodes,
// looked up by its ZFS name, and reports whether the name is one of them.
//
// Both halves of the Property matter to a caller deciding whether a change is
// needed. Value is the effective setting; Source distinguishes a value set on
// this dataset ("LOCAL") from one that merely follows the parent ("INHERITED",
// "DEFAULT") — which is the difference between "already disabled" and
// "inheriting a parent that happens to be disabled today".
func (d *Dataset) ZFSProperty(name string) (Property, bool) {
	if d == nil {
		return Property{}, false
	}
	switch name {
	case "sync":
		return d.Sync.Property, true
	case "compression":
		return d.Compression.Property, true
	case "atime":
		return d.ATime.Property, true
	case "recordsize":
		return d.RecordSize.Property, true
	}
	return Property{}, false
}

// Owned reports whether this driver created the dataset, requiring the marker to
// be set LOCALly rather than inherited from a parent.
func (d *Dataset) Owned(property, value string) bool {
	if d == nil || d.UserProperties == nil {
		return false
	}
	p, ok := d.UserProperties[property]
	return ok && p.Value == value && p.Source == "LOCAL"
}

// Snapshot is a ZFS snapshot.
type Snapshot struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dataset   string `json:"dataset"`
	CreateTXG string `json:"createtxg"`
	Used      sizeField

	// Properties is the ZFS property block pool.snapshot.query returns. Only
	// the two fields below are decoded from it, because the rest of the block
	// is large and the driver reads none of it.
	Properties SnapshotProperties `json:"properties"`
}

// SnapshotProperties is the subset of a snapshot's ZFS properties the driver
// reads back.
type SnapshotProperties struct {
	// Creation is unix seconds. ZFS records it on the snapshot itself, which is
	// the only creation time that survives a controller restart — a snapshot
	// this driver did not just take has no other source for it.
	Creation sizeField `json:"creation"`
	// VolSize is the provisioned size of a zvol snapshot. It is ABSENT on a
	// filesystem snapshot (and refquota is not carried on a snapshot at all),
	// so a filesystem's provisioned size has to come from its live dataset.
	VolSize sizeField `json:"volsize"`
}

// CreationTime is the snapshot's ZFS creation time, or the zero time when the
// appliance did not report one.
func (s Snapshot) CreationTime() time.Time {
	if s.Properties.Creation.Parsed <= 0 {
		return time.Time{}
	}
	return time.Unix(s.Properties.Creation.Parsed, 0).UTC()
}

// Pool is a ZFS pool.
type Pool struct {
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Healthy bool      `json:"healthy"`
	Size    sizeField `json:"size"`
	Free    sizeField `json:"free"`
}

// NFSShare is an NFS export.
type NFSShare struct {
	ID       int      `json:"id"`
	Path     string   `json:"path"`
	Networks []string `json:"networks"`
	Enabled  bool     `json:"enabled"`
}

// ISCSIExtent is an iSCSI extent backed by a zvol.
type ISCSIExtent struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Disk string `json:"disk"`
	NAA  string `json:"naa"`
}

// ISCSITarget is an iSCSI target.
type ISCSITarget struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ISCSITargetExtent maps an extent into a target at a LUN id.
type ISCSITargetExtent struct {
	ID     int `json:"id"`
	Target int `json:"target"`
	Extent int `json:"extent"`
	LUNID  int `json:"lunid"`
}

// ISCSIPortal is a listening portal.
type ISCSIPortal struct {
	ID      int    `json:"id"`
	Comment string `json:"comment"`
}

// ISCSIGlobal is the global iSCSI configuration.
type ISCSIGlobal struct {
	Basename string `json:"basename"`
	Port     int    `json:"listen_port"`
	ALUA     bool   `json:"alua"`
}

// LocalProperty returns a user property's value when it was set on this dataset
// itself, and "" when it is absent or merely inherited from a parent.
//
// Inheritance is the reason this is not a plain map lookup: a property a child
// inherited says something about its parent, not about the child.
func (d *Dataset) LocalProperty(key string) string {
	if d == nil || d.UserProperties == nil {
		return ""
	}
	p, ok := d.UserProperties[key]
	if !ok || p.Source != "LOCAL" {
		return ""
	}
	return p.Value
}
