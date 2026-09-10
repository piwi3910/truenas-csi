package main

import (
	"strings"
	"testing"
)

// TestPlainTextRendersApplianceAlertsForATerminal.
//
// TrueNAS writes alert text as HTML for its own web UI, and printing it into a
// tab-separated table put raw markup in front of an operator and broke the
// column alignment with embedded newlines. The input here is the REST
// deprecation notice exactly as a live 25.10.6 appliance emitted it.
func TestPlainTextRendersApplianceAlertsForATerminal(t *testing.T) {
	const real = "The deprecated REST API was used to authenticate 14 times in the last " +
		"24 hours from the following IP addresses:<br>192.168.10.212.<br>The REST API " +
		"will be removed in version 26.04. To avoid service disruption, migrate any " +
		"remaining integrations to the supported JSON-RPC 2.0 over WebSocket API before " +
		"upgrading. For migration guidance, see the " +
		`<a href="https://api.truenas.com/v25.10/jsonrpc.html" target="_blank">documentation</a>.`

	got := plainText(real)
	for _, unwanted := range []string{"<br>", "<a ", "</a>", "href="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("rendered alert still contains %q: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, "192.168.10.212") {
		t.Error("the addresses the alert is about were lost")
	}
	if !strings.Contains(got, "documentation") {
		t.Error("the link text was lost; only the markup should go")
	}
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Errorf("a table cell must not contain newlines or tabs: %q", got)
	}

	// Entities the middleware escapes come back as themselves.
	if got := plainText("pool &amp; disk"); got != "pool & disk" {
		t.Errorf("plainText(entities) = %q", got)
	}
	// Anything that is not one of the constructs the appliance emits is left
	// alone rather than silently deleted.
	if got := plainText("a < b and c > d"); got != "a < b and c > d" {
		t.Errorf("plainText left-alone case = %q", got)
	}
}
