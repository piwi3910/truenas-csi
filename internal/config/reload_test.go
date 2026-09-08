package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustLoadProjected(t *testing.T, dir, content string) (string, *Config) {
	t.Helper()
	path := projectSecret(t, dir, "config.yaml", content, 1)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("loading the starting configuration: %v", err)
	}
	return path, cfg
}

// TestReloadAdoptsARotatedCredential is the happy path: the API key changed and
// nothing else, so the driver takes it without a restart.
func TestReloadAdoptsARotatedCredential(t *testing.T) {
	dir := t.TempDir()
	path, cfg := mustLoadProjected(t, dir, fmt.Sprintf(validConfig, "8-first"))
	r := NewReloader(path, cfg)

	projectSecret(t, dir, "config.yaml", fmt.Sprintf(validConfig, "9-rotated"), 2)

	rotated, next, err := r.Reload()
	if err != nil {
		t.Fatalf("a credential-only change must be accepted, got %v", err)
	}
	if len(rotated) != 1 {
		t.Fatalf("want exactly one rotated backend, got %v", rotated)
	}
	if got := rotated["nas1"].APIKey; got != "9-rotated" {
		t.Fatalf("rotated key = %q, want %q", got, "9-rotated")
	}
	if next.Backends["nas1"].APIKey != "9-rotated" {
		t.Fatal("the adopted config does not carry the new key")
	}
	if r.Current() != next {
		t.Fatal("Current() must be the configuration just adopted")
	}
}

// TestReloadLeavesTheRunningConfigIntact is the test the whole design exists
// for. Every one of these edits must be refused, and after each refusal the
// driver must still be running on exactly the configuration it had — a bad edit
// to a mounted Secret cannot be allowed to take the driver down or to
// half-apply.
func TestReloadLeavesTheRunningConfigIntact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		// wantIn is a fragment the operator must see in the error.
		wantIn string
		// wantRestart is true when the edit is valid but not reloadable.
		wantRestart bool
	}{
		{
			name: "plaintext endpoint",
			content: `
backends:
  nas1:
    endpoint: http://192.168.10.253/api/current
    username: truenas_admin
    apiKey: 9-rotated
    pool: Pool0
    parentDataset: k8s
`,
			wantIn: "wss://",
		},
		{
			name:    "unparsable yaml",
			content: "backends: [this is not a map\n",
			wantIn:  "parse config",
		},
		{
			name: "missing api key",
			content: `
backends:
  nas1:
    endpoint: wss://192.168.10.253/api/current
    username: truenas_admin
    pool: Pool0
    parentDataset: k8s
`,
			wantIn: "apiKey must be set",
		},
		{
			name:        "endpoint repointed at another appliance",
			content:     strings.Replace(fmt.Sprintf(validConfig, "8-first"), "192.168.10.253", "192.168.10.254", 1),
			wantIn:      "nas1.endpoint",
			wantRestart: true,
		},
		{
			name:        "pool changed",
			content:     strings.Replace(fmt.Sprintf(validConfig, "8-first"), "Pool0", "Pool1", 1),
			wantIn:      "nas1.pool",
			wantRestart: true,
		},
		{
			name: "backend removed",
			content: `
backends:
  nas2:
    endpoint: wss://192.168.10.253/api/current
    username: truenas_admin
    apiKey: 8-first
    pool: Pool0
    parentDataset: k8s
`,
			wantIn:      "nas1.(removed)",
			wantRestart: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, cfg := mustLoadProjected(t, dir, fmt.Sprintf(validConfig, "8-first"))
			r := NewReloader(path, cfg)

			projectSecret(t, dir, "config.yaml", tc.content, 2)

			rotated, next, err := r.Reload()
			if err == nil {
				t.Fatalf("this edit must be refused, but it was adopted: %+v", next)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("the error must tell the operator what is wrong: want %q in %v", tc.wantIn, err)
			}
			if tc.wantRestart && !errors.Is(err, ErrRestartRequired) {
				t.Errorf("want ErrRestartRequired, got %v", err)
			}
			if rotated != nil || next != nil {
				t.Error("a refused reload must return nothing to apply")
			}
			if r.Current() != cfg {
				t.Fatal("the running configuration was disturbed by a refused reload")
			}
			if got := r.Current().Backends["nas1"].APIKey; got != "8-first" {
				t.Fatalf("running credential changed to %q after a refused reload", got)
			}
		})
	}
}

// TestReloadIgnoresANoOpRewrite keeps a kubelet re-projection that changed
// nothing from dropping healthy connections.
func TestReloadIgnoresANoOpRewrite(t *testing.T) {
	dir := t.TempDir()
	path, cfg := mustLoadProjected(t, dir, fmt.Sprintf(validConfig, "8-first"))
	r := NewReloader(path, cfg)

	projectSecret(t, dir, "config.yaml", fmt.Sprintf(validConfig, "8-first"), 2)

	rotated, _, err := r.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if len(rotated) != 0 {
		t.Fatalf("identical content must rotate nothing, got %v", rotated)
	}
}

// TestReloaderRunReportsRotationsAndRefusals exercises the whole path the
// driver uses: watch, re-read, and either hand over the credential or complain.
func TestReloaderRunReportsRotationsAndRefusals(t *testing.T) {
	dir := t.TempDir()
	path, cfg := mustLoadProjected(t, dir, fmt.Sprintf(validConfig, "8-first"))
	r := NewReloader(path, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rotations := make(chan map[string]Credential, 4)
	failures := make(chan error, 4)
	go func() {
		_ = r.Run(ctx,
			func(rotated map[string]Credential, _ *Config) { rotations <- rotated },
			func(err error) { failures <- err })
	}()
	time.Sleep(200 * time.Millisecond)

	projectSecret(t, dir, "config.yaml", fmt.Sprintf(validConfig, "9-rotated"), 2)
	select {
	case got := <-rotations:
		if got["nas1"].APIKey != "9-rotated" {
			t.Fatalf("rotated credential = %+v", got["nas1"])
		}
	case err := <-failures:
		t.Fatalf("a credential rotation was refused: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no rotation was reported")
	}

	// And now a broken edit, which must be reported and change nothing.
	projectSecret(t, dir, "config.yaml", "backends: [broken\n", 3)
	select {
	case err := <-failures:
		if !strings.Contains(err.Error(), "parse config") {
			t.Fatalf("unexpected failure: %v", err)
		}
	case got := <-rotations:
		t.Fatalf("a broken config was adopted: %v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("a broken config change was never reported")
	}
	if got := r.Current().Backends["nas1"].APIKey; got != "9-rotated" {
		t.Fatalf("running credential is %q; the broken edit disturbed it", got)
	}
}

func TestCheckReloadableAcceptsCredentialOnlyChanges(t *testing.T) {
	base := func() *Config {
		return &Config{
			Backends: map[string]Backend{"nas1": {
				Name: "nas1", Endpoint: "wss://nas/api/current", Username: "u",
				APIKey: "8-a", Pool: "Pool0", ParentDataset: "k8s",
			}},
		}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"api key", func(c *Config) { b := c.Backends["nas1"]; b.APIKey = "9-b"; c.Backends["nas1"] = b }, false},
		{"username", func(c *Config) { b := c.Backends["nas1"]; b.Username = "other"; c.Backends["nas1"] = b }, false},
		{"ca cert", func(c *Config) { b := c.Backends["nas1"]; b.CACert = []byte("pem"); c.Backends["nas1"] = b }, false},
		{"skip verify", func(c *Config) {
			b := c.Backends["nas1"]
			b.InsecureSkipVerify = true
			c.Backends["nas1"] = b
		}, false},
		{"log level", func(c *Config) { c.LogLevel = "debug" }, false},
		{"metrics addr", func(c *Config) { c.MetricsAddr = ":9999" }, true},
		{"metrics interval", func(c *Config) { c.MetricsInterval = "5m" }, true},
		{"reserved bytes", func(c *Config) {
			b := c.Backends["nas1"]
			b.ReservedBytes = 1
			c.Backends["nas1"] = b
		}, true},
		{"rate limit", func(c *Config) { b := c.Backends["nas1"]; b.RateLimit = 5; c.Backends["nas1"] = b }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur, next := base(), base()
			tc.mutate(next)
			err := CheckReloadable(cur, next)
			if tc.wantErr && !errors.Is(err, ErrRestartRequired) {
				t.Fatalf("want ErrRestartRequired, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want the change accepted, got %v", err)
			}
		})
	}
}

// TestCheckReloadableErrorNamesEveryField saves an operator from fixing one
// field at a time.
func TestCheckReloadableErrorNamesEveryField(t *testing.T) {
	cur := &Config{Backends: map[string]Backend{"nas1": {
		Name: "nas1", Endpoint: "wss://a/api/current", Pool: "Pool0", ParentDataset: "k8s",
	}}}
	next := &Config{MetricsAddr: ":1", Backends: map[string]Backend{"nas1": {
		Name: "nas1", Endpoint: "wss://b/api/current", Pool: "Pool1", ParentDataset: "k8s",
	}}}
	err := CheckReloadable(cur, next)
	for _, want := range []string{"metricsAddr", "nas1.endpoint", "nas1.pool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
}

// TestCheckReloadableNeverLeaksValues: the fields it reports sit next to an API
// key, and a diff that prints values is one refactor away from printing that.
func TestCheckReloadableNeverLeaksValues(t *testing.T) {
	cur := &Config{Backends: map[string]Backend{"nas1": {
		Name: "nas1", Endpoint: "wss://a/api/current", APIKey: "8-secret", Pool: "Pool0",
	}}}
	next := &Config{Backends: map[string]Backend{"nas1": {
		Name: "nas1", Endpoint: "wss://b/api/current", APIKey: "9-alsosecret", Pool: "Pool1",
	}}}
	err := CheckReloadable(cur, next)
	for _, secret := range []string{"8-secret", "9-alsosecret", "wss://a", "wss://b"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal message contains a configured value %q: %v", secret, err)
		}
	}
}
