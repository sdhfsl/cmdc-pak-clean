package main

import "testing"

func TestResolveModelCatalogFirst(t *testing.T) {
	// Seed the catalog the way loadModelCatalogAt would from harness data.
	modelMu.Lock()
	parsedModels = []modelSpec{
		{ID: "meta/muse-spark-1.3", ContextWindow: 1048576, Effort: []string{"low", "medium", "high", "xhigh", "max"}},
		{ID: "meta/muse-spark-1.3-contributor", ContextWindow: 1048576, Effort: []string{"low", "medium", "high", "xhigh"}},
	}
	effortSupported = map[string]bool{"meta/muse-spark-1.3": true, "meta/muse-spark-1.3-contributor": true}
	modelMu.Unlock()
	// Full ids present in the catalog pass through untouched, even when
	// their suffix collides with a short alias.
	if got := resolveModel("meta/muse-spark-1.3"); got != "meta/muse-spark-1.3" {
		t.Errorf("full id rewritten: %q", got)
	}
	// Short aliases still resolve.
	if got := resolveModel("muse-spark-1.3"); got != "meta/muse-spark-1.3-contributor" {
		t.Errorf("short alias broken: %q", got)
	}
	// Unknown ids pass through (upstream decides).
	if got := resolveModel("foo/bar-1.0"); got != "foo/bar-1.0" {
		t.Errorf("unknown id mangled: %q", got)
	}
	// Case-insensitive full id still passes through.
	if got := resolveModel("Meta/Muse-Spark-1.3"); got != "Meta/Muse-Spark-1.3" {
		t.Errorf("cased full id rewritten: %q", got)
	}
	t.Logf("resolve: full-id passthrough + short-alias mapping both OK")
}
