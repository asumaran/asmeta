package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBranchTicket(t *testing.T) {
	work := t.TempDir()
	for _, d := range []string{"SYF2-5895", "jobs-inbox"} {
		if err := os.Mkdir(filepath.Join(work, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(t.TempDir(), "asdev.local.md")
	doc := "---\nstacks:\n  mas:\n    jira:\n      base_url: https://x.atlassian.net\n      default_project: ESHOP\n---\n"
	if err := os.WriteFile(cfg, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	old := workDir
	workDir = work
	t.Cleanup(func() { workDir = old })
	t.Setenv("ASDEV_CONFIG", cfg)
	t.Setenv("ASMETA_TICKET_KEYS", " abc ,")

	for _, c := range []struct{ branch, want string }{
		{"feat-ESHOP-1270-front-platform-headers", "ESHOP-1270"},
		{"SYF2-6283", "SYF2-6283"},
		{"coder-syf2-6278-chat-approve-resolve-record", "SYF2-6278"},
		{"cursor/syf2-6009-linkedin-published-at-20261108", "SYF2-6009"},
		{"Syf2-6402", "SYF2-6402"},
		{"feature/eshop-12-foo", "ESHOP-12"},
		{"fix-3-abc-7", "ABC-7"},
		{"inbox-27-asreviewer-finish-failed", ""},
		{"fix-123", ""},
		{"myeshop-12", ""},
		{"9eshop-12", ""},
		{"claude-persona-eval-tooling", ""},
		{"", ""},
	} {
		if got := branchTicket(c.branch); got != c.want {
			t.Errorf("branchTicket(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}
