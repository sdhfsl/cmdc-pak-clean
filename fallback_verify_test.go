package main

import "testing"

// Verify the fallback catalog carries metadata (never bare IDs).
func TestFallbackCatalogHasMetadata(t *testing.T) {
	got := fallbackCatalog()
	if len(got) != len(fallbackSpecs) {
		t.Fatalf("got %d models, want %d", len(got), len(fallbackSpecs))
	}
	for _, m := range got {
		if m.ID == "" {
			t.Error("empty model id in fallback")
		}
	}
	byID := map[string]modelSpec{}
	for _, m := range got {
		byID[m.ID] = m
	}
	must := map[string]struct {
		ctx int
		eff []string
	}{
		"meta/muse-spark-1.3-contributor": {1048576, []string{"low", "medium", "high", "xhigh"}},
		"deepseek/deepseek-v4.1-flash":    {1000000, []string{"low", "high", "max"}},
		"z-ai/glm-5.3-flash":              {1048576, []string{"low", "high", "max"}},
	}
	for id, want := range must {
		m, ok := byID[id]
		if !ok {
			t.Errorf("%s missing from fallback", id)
			continue
		}
		if m.ContextWindow != want.ctx {
			t.Errorf("%s: ctx=%d want %d", id, m.ContextWindow, want.ctx)
		}
		if len(m.Effort) != len(want.eff) {
			t.Errorf("%s: effort=%v want %v", id, m.Effort, want.eff)
		}
	}
	t.Logf("fallback: %d models, all with metadata", len(got))
}
