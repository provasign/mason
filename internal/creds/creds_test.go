package creds

import (
	"strings"
	"testing"
)

func TestKeyPageURL(t *testing.T) {
	for _, vendor := range []string{"anthropic", "openai"} {
		url := KeyPageURL(vendor)
		if !strings.HasPrefix(url, "https://") {
			t.Errorf("KeyPageURL(%q) = %q", vendor, url)
		}
	}
	if KeyPageURL("nope") != "" {
		t.Fatal("unknown vendor must yield empty URL")
	}
}

func TestOpenKeyPageUnknownVendor(t *testing.T) {
	// Unknown vendors must not attempt a browser launch.
	if url, opened := OpenKeyPage("nope"); url != "" || opened {
		t.Fatalf("unknown vendor: %q %v", url, opened)
	}
}
