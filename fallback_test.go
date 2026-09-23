package main

import (
	"os"
	"path/filepath"
	"testing"
)

func withIsolatedAuth(t *testing.T, setup func(dir string)) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CMDC_PAK_AUTH_FILE", filepath.Join(dir, "no-such-file.json"))
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
	// clear cache
	authCache.Lock()
	authCache.a, authCache.path, authCache.err = nil, "", nil
	authCache.exp = authCache.exp.AddDate(-1, 0, 0)
	authCache.Unlock()
	setup(dir)
}

func TestConfigTokenFallback(t *testing.T) {
	withIsolatedAuth(t, func(dir string) {
		_ = os.Setenv("CMDC_PAK_TOKEN", "tok-from-env")
		defer os.Unsetenv("CMDC_PAK_TOKEN")
		cfgMu.Lock()
		saved := cfg.Token
		cfg.Token = ""
		cfgMu.Unlock()
		defer func() {
			cfgMu.Lock()
			cfg.Token = saved
			cfgMu.Unlock()
		}()
		a, path, err := loadLocalCommandCodeAuth()
		if err != nil {
			t.Fatalf("expected fallback auth, got err %v", err)
		}
		if a.ApiKey != "tok-from-env" || path != "config" {
			t.Fatalf("got %q %q", a.ApiKey, path)
		}
	})
}

func TestConfigTokenDashboardSaved(t *testing.T) {
	withIsolatedAuth(t, func(dir string) {
		os.Unsetenv("CMDC_PAK_TOKEN")
		cfgMu.Lock()
		saved := cfg.Token
		cfg.Token = "tok-from-dashboard"
		cfgMu.Unlock()
		defer func() {
			cfgMu.Lock()
			cfg.Token = saved
			cfgMu.Unlock()
		}()
		a, path, err := loadLocalCommandCodeAuth()
		if err != nil {
			t.Fatalf("expected fallback auth, got err %v", err)
		}
		if a.ApiKey != "tok-from-dashboard" || path != "config" {
			t.Fatalf("got %q %q", a.ApiKey, path)
		}
	})
}

func TestNoFallbackWithoutToken(t *testing.T) {
	withIsolatedAuth(t, func(dir string) {
		os.Unsetenv("CMDC_PAK_TOKEN")
		cfgMu.Lock()
		saved := cfg.Token
		cfg.Token = ""
		cfgMu.Unlock()
		defer func() {
			cfgMu.Lock()
			cfg.Token = saved
			cfgMu.Unlock()
		}()
		_, _, err := loadLocalCommandCodeAuth()
		if err == nil {
			t.Fatal("expected error when nothing configured")
		}
	})
}
