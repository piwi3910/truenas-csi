package config

import (
	"strings"
	"testing"
)

// TestRejectsPoolPrefixedParentDataset pins that a parentDataset written as a
// path is refused at configuration time.
//
// Every install example in this repository once said "tank/k8s", which the
// driver reads as tank/tank/k8s. Nothing rejected it at startup: the driver
// came up healthy and failed the operator's first PVC with a message naming a
// dataset component rather than the setting or the file. The message here has
// to name the fix, because the person reading it is looking at their values
// file, not at volume/id.go.
func TestRejectsPoolPrefixedParentDataset(t *testing.T) {
	for _, tc := range []struct{ name, pool, parent string }{
		{"pool prefixed", "tank", "tank/k8s"},
		{"any path", "tank", "a/b"},
		{"backslash", "tank", `a\b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{
				NodeID: "n",
				Backends: map[string]Backend{"nas1": {
					Name: "nas1", Endpoint: "wss://nas.example.com/api/current",
					Username: "truenas_admin", APIKey: "k",
					Pool: tc.pool, ParentDataset: tc.parent,
				}},
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("parentDataset %q was accepted; the driver would start and then fail "+
					"the first PVC with a message about %q", tc.parent, tc.pool+"/"+tc.parent)
			}
			if !strings.Contains(err.Error(), "parentDataset") {
				t.Errorf("the refusal does not name the setting the operator has to change: %v", err)
			}
		})
	}

	// The correct shape still validates.
	c := &Config{
		NodeID: "n",
		Backends: map[string]Backend{"nas1": {
			Name: "nas1", Endpoint: "wss://nas.example.com/api/current",
			Username: "truenas_admin", APIKey: "k", Pool: "tank", ParentDataset: "k8s",
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a single-component parentDataset must be valid: %v", err)
	}
}
