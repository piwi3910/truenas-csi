package config

import (
	"errors"
	"strings"
	"testing"
)

func TestQuotaFor(t *testing.T) {
	q := NamespaceQuotas{
		Enabled:      true,
		DefaultBytes: 10 << 30,
		PerNamespace: map[string]int64{"team-a": 100 << 30, "sandbox": 0},
	}
	cases := []struct {
		name      string
		namespace string
		want      int64
	}{
		{name: "no entry falls back to the default", namespace: "team-b", want: 10 << 30},
		{name: "an override wins", namespace: "team-a", want: 100 << 30},
		{name: "an explicit zero is unlimited, not absent", namespace: "sandbox", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := q.QuotaFor(tc.namespace); got != tc.want {
				t.Fatalf("QuotaFor(%q) = %d, want %d", tc.namespace, got, tc.want)
			}
		})
	}
}

func TestNamespaceQuotasValidate(t *testing.T) {
	cases := []struct {
		name    string
		q       NamespaceQuotas
		wantErr string
	}{
		{name: "the zero value is a valid disabled feature"},
		{name: "enabled with no quotas is unlimited accounting",
			q: NamespaceQuotas{Enabled: true}},
		{name: "enabled with quotas",
			q: NamespaceQuotas{Enabled: true, DefaultBytes: 1 << 30,
				PerNamespace: map[string]int64{"team-a": 2 << 30}}},
		{name: "quotas configured but the feature left off",
			q:       NamespaceQuotas{DefaultBytes: 1 << 30},
			wantErr: "enabled is false"},
		{name: "negative default",
			q:       NamespaceQuotas{Enabled: true, DefaultBytes: -1},
			wantErr: "must not be negative"},
		{name: "negative override",
			q:       NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{"team-a": -1}},
			wantErr: "must not be negative"},
		{name: "a namespace key with a path separator would name another dataset",
			q:       NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{"team-a/evil": 1}},
			wantErr: "DNS-1123"},
		{name: "an uppercase namespace can never match a real one",
			q:       NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{"TeamA": 1}},
			wantErr: "DNS-1123"},
		{name: "an over-long namespace key",
			q:       NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{strings.Repeat("a", 64): 1}},
			wantErr: "limit 63"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.q.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestNamespaceQuotasNeedARestart pins the reload rule. The controller captured
// this configuration at startup, so adopting a layout change under a running
// driver would put two volumes of one namespace in two different places.
func TestNamespaceQuotasNeedARestart(t *testing.T) {
	base := func(q NamespaceQuotas) *Config {
		return &Config{NodeID: "worker-21", Backends: map[string]Backend{
			"nas1": {Name: "nas1", Endpoint: "wss://nas1.example.com/api/current",
				Username: "csi", APIKey: "8-x", Pool: "Pool0", ParentDataset: "k8s",
				NamespaceQuotas: q},
		}}
	}
	cases := []struct {
		name        string
		cur, next   NamespaceQuotas
		wantRestart bool
	}{
		{name: "unchanged", cur: NamespaceQuotas{Enabled: true, DefaultBytes: 1},
			next: NamespaceQuotas{Enabled: true, DefaultBytes: 1}},
		{name: "nil and empty maps are the same configuration",
			cur:  NamespaceQuotas{Enabled: true},
			next: NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{}}},
		{name: "switched on", next: NamespaceQuotas{Enabled: true}, wantRestart: true},
		{name: "default changed", cur: NamespaceQuotas{Enabled: true, DefaultBytes: 1},
			next: NamespaceQuotas{Enabled: true, DefaultBytes: 2}, wantRestart: true},
		{name: "override added", cur: NamespaceQuotas{Enabled: true},
			next:        NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{"team-a": 1}},
			wantRestart: true},
		{name: "override changed",
			cur:         NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{"team-a": 1}},
			next:        NamespaceQuotas{Enabled: true, PerNamespace: map[string]int64{"team-a": 2}},
			wantRestart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckReloadable(base(tc.cur), base(tc.next))
			if got := errors.Is(err, ErrRestartRequired); got != tc.wantRestart {
				t.Fatalf("CheckReloadable = %v, want restart required = %v", err, tc.wantRestart)
			}
			if tc.wantRestart && !strings.Contains(err.Error(), "nas1.namespaceQuotas") {
				t.Fatalf("error must name the field, got %v", err)
			}
		})
	}
}
