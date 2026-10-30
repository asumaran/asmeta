package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects logf's stderr until the returned function is called.
func captureLog(t *testing.T) func() string {
	t.Helper()
	prev := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	restored := false
	t.Cleanup(func() {
		if !restored {
			os.Stderr = prev
		}
	})
	return func() string {
		w.Close()
		os.Stderr = prev
		restored = true
		data, _ := io.ReadAll(r)
		return string(data)
	}
}

func TestParseGHAccounts(t *testing.T) {
	done := captureLog(t)
	orgs, def := parseGHAccounts("# gh account per org\n\n  # indented comment\r\n" +
		"SyftData alfredo-syft\r\nsyftdata second-wins-not\n" +
		"one-field\nthree fields here\n" +
		"default  asumaran\ndefault second\nacme corp\n")
	log := done()
	if len(orgs) != 2 || orgs["syftdata"] != "alfredo-syft" || orgs["acme"] != "corp" {
		t.Errorf("orgs = %v", orgs)
	}
	if def != "asumaran" {
		t.Errorf("default = %q (first match wins)", def)
	}
	if !strings.Contains(log, "one-field") || !strings.Contains(log, "three fields here") {
		t.Errorf("malformed lines must be warned about: %q", log)
	}
}

func TestAccountFor(t *testing.T) {
	orgs := map[string]string{"syftdata": "alfredo-syft"}
	if a := accountFor(orgs, "asumaran", "SyftData"); a != "alfredo-syft" {
		t.Errorf("mapped org, case-insensitive: %q", a)
	}
	if a := accountFor(orgs, "asumaran", "other"); a != "asumaran" {
		t.Errorf("unmapped org falls back to default: %q", a)
	}
	if a := accountFor(orgs, "", "other"); a != "" {
		t.Errorf("no default resolves to nothing: %q", a)
	}
	if a := accountFor(nil, "", "any"); a != "" {
		t.Errorf("no map resolves to nothing: %q", a)
	}
}

func TestLoadGHAccountsAbsent(t *testing.T) {
	t.Setenv("GH_ACCOUNTS_MAP", filepath.Join(t.TempDir(), "absent"))
	done := captureLog(t)
	orgs, def := loadGHAccounts()
	if log := done(); orgs != nil || def != "" || log != "" {
		t.Errorf("an absent map degrades silently: orgs=%v def=%q log=%q", orgs, def, log)
	}
}

func TestLoadGHAccountsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-accounts")
	if err := os.WriteFile(path, []byte("syftdata alfredo-syft\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ACCOUNTS_MAP", path)
	done := captureLog(t)
	orgs, def := loadGHAccounts()
	if log := done(); orgs != nil || def != "" || !strings.Contains(log, "unreadable") {
		t.Errorf("an unreadable map is logged: orgs=%v def=%q log=%q", orgs, def, log)
	}
}

func TestLoadGHAccountsEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "map")
	if err := os.WriteFile(path, []byte("syftdata alfredo-syft\ndefault asumaran\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ACCOUNTS_MAP", path)
	orgs, def := loadGHAccounts()
	if orgs["syftdata"] != "alfredo-syft" || def != "asumaran" {
		t.Errorf("GH_ACCOUNTS_MAP is respected: orgs=%v def=%q", orgs, def)
	}
}

func TestLoadGHAccountsDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GH_ACCOUNTS_MAP", "   ") // blank = unset
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "gh-accounts")
	if err := os.WriteFile(path, []byte("acme corp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orgs, def := loadGHAccounts()
	if orgs["acme"] != "corp" || def != "" {
		t.Errorf("a blank override falls back to ~/.config/gh-accounts: orgs=%v def=%q", orgs, def)
	}
}
