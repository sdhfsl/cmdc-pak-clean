package main

import (
	"testing"
	"time"
)

// True fallback: harness unreadable AND memory empty -> persisted value.
func TestCLIVersionTrueFallback(t *testing.T) {
	t.Setenv("CMDC_HARNESS_PATH", "C:/definitely-not-here-xyz/index.js")
	t.Setenv("LOCALAPPDATA", "C:/definitely-not-here-xyz")
	t.Setenv("USERPROFILE", "C:/definitely-not-here-xyz")
	t.Setenv("HOME", "C:/definitely-not-here-xyz")

	cfgMu.Lock()
	savedTok, savedVer := cfg.Token, cfg.CliVersion
	cfg.Token = ""
	cfg.CliVersion = "7.77.7-stale-but-real"
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfg.Token, cfg.CliVersion = savedTok, savedVer
		cfgMu.Unlock()
	}()

	// NOTE: LOCALAPPDATA fallback may still resolve on this machine; the
	// assertion below only holds if it doesn't. Log both outcomes.
	cliVersionMu.Lock()
	cliVersion = ""
	cliVersionAt = time.Now().Add(-2 * cliVersionTTL)
	cliVersionMu.Unlock()

	v := cliVersionGet()
	t.Logf("harness-unreadable version = %q", v)
	if v == "" {
		t.Fatal("must never return empty: gateway 403s the omit")
	}
	if v != "7.77.7-stale-but-real" {
		t.Fatalf("expected persisted value, got %q", v)
	}
}
