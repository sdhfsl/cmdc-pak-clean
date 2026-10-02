package main

import (
	"strings"
	"testing"
)

// withCatalogEntry installs a minimal model catalog for the duration of a test.
func withCatalogEntry(t *testing.T, spec modelSpec) {
	t.Helper()
	modelMu.Lock()
	saved := parsedModels
	parsedModels = []modelSpec{spec}
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		parsedModels = saved
		modelMu.Unlock()
	})
}

func TestEstimateInputTokensClasses(t *testing.T) {
	// CJK: ~0.95 token per rune. Must cover the live measurement (0.90) and
	// stay within a sane overcount band.
	cjk := strings.Repeat("字", 10000) // 30KB, ~9000 real tokens
	if got := estimateInputTokens(cjk); got < 9000 || got > 13000 {
		t.Errorf("cjk estimate = %d, want 9000..13000", got)
	}
	// Base64 image data URLs are priced by pixels upstream and must not be
	// counted as text bytes.
	img := "data:image/png;base64," + strings.Repeat("A", 50000)
	if got := estimateInputTokens(map[string]any{"image": img}); got > 100 {
		t.Errorf("image data counted: %d", got)
	}
	// Whitespace is far cheaper than punctuation.
	sp := estimateInputTokens(strings.Repeat(" ", 5000))
	pu := estimateInputTokens(strings.Repeat("{", 5000))
	if sp >= pu/4 {
		t.Errorf("space estimate %d not clearly below punctuation %d", sp, pu)
	}
	// A dense alphanumeric run (base64-like) costs more per byte than a
	// dictionary-word stream of the same length.
	dense := estimateInputTokens(strings.Repeat("A1b2C3d4", 500))
	words := estimateInputTokens(strings.Repeat("service ", 500))
	if dense <= words {
		t.Errorf("dense run %d should exceed word stream %d", dense, words)
	}
}

func TestFitContextBudget(t *testing.T) {
	withCatalogEntry(t, modelSpec{ID: "test/model", ContextWindow: 1000000})

	// Small payload: untouched.
	if fit := fitContextBudget("test/model", 128000, strings.Repeat("hi ", 1000)); fit.Shrunk {
		t.Errorf("small payload shrunk: %+v", fit)
	}
	// Unknown model: untouched even for huge payloads.
	if fit := fitContextBudget("mystery/model", 128000, strings.Repeat("字", 1000000)); fit.Shrunk {
		t.Errorf("unknown model shrunk: %+v", fit)
	}
	// History filling most of the window (the dead-conversation case): the
	// budget shrinks to the room that is left, well above the floor.
	cjk := strings.Repeat("字", 920000)
	fit := fitContextBudget("test/model", 128000, cjk)
	if !fit.Shrunk || fit.Over {
		t.Fatalf("expected graceful shrink, got %+v", fit)
	}
	if fit.Fitted < 40000 || fit.Fitted > 128000 {
		t.Errorf("fitted = %d, want a usable budget in the tens of thousands", fit.Fitted)
	}
	// History beyond the window: floor budget plus the Over warning.
	over := strings.Repeat("字", 1100000)
	fit2 := fitContextBudget("test/model", 128000, over)
	if fit2.Fitted != minCompletionBudget || !fit2.Over {
		t.Errorf("over-limit fit = %+v, want floor %d with Over", fit2, minCompletionBudget)
	}
	// A small client budget is never raised by the fit.
	if fit3 := fitContextBudget("test/model", 512, cjk); fit3.Shrunk {
		t.Errorf("small budget must not be touched: %+v", fit3)
	}
}
