package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessFor(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "wt", "monorepo-front", "feat-ESHOP-1270")
	taskDir := filepath.Join(dir, "work", "ESHOP-1270")
	for _, d := range []string{worker, taskDir, filepath.Join(dir, "lineage", "archive")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link-to-worker")
	if err := os.Symlink(worker, link); err != nil {
		t.Fatal(err)
	}

	lin := filepath.Join(dir, "lineage")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(lin, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("es-1270-f.json", `{"agent":{"name":"es-1270-f","workspace_id":"w1","worktree":"`+worker+`"},"task":{"ref":"ESHOP-1270#F"}}`)
	// Shared workspace: the coordinator's tab lives in the worktree's
	// workspace (w3) next to its workers. The worker must win, and among
	// workers the most recently launched.
	write("co_ab12.json", `{"agent":{"name":"co_ab12","workspace_id":"w3","worktree":"`+taskDir+`"},"task":{"ref":"ESHOP-9"},"launched_at":"2026-10-17T12:00:00"}`)
	write("wk_cd34.json", `{"agent":{"name":"wk_cd34","workspace_id":"w3","worktree":"`+worker+`"},"task":{"ref":"ESHOP-9#A"},"launched_at":"2026-10-16T12:00:00"}`)
	write("wk_ef56.json", `{"agent":{"name":"wk_ef56","workspace_id":"w3","worktree":"`+worker+`"},"task":{"ref":"ESHOP-9#B"},"launched_at":"2026-10-17T09:00:00"}`)
	write("es-1270.json", `{"agent":{"name":"es-1270","workspace_id":"w2","worktree":"`+taskDir+`"},"task":{"ref":"ESHOP-1270"}}`)
	write("legacy.json", `{"agent":{"name":"legacy","worktree":"`+worker+`"},"task":{"ref":"LEGACY-1"}}`)
	write("broken.json", `{`)
	write("es-1270-f.lock", "")
	write(filepath.Join("archive", "old.json"), `{"agent":{"name":"old","workspace_id":"w1","worktree":"`+worker+`"},"task":{"ref":"OLD-1"}}`)

	// Records whose agent herdr no longer lists (its pane closed, or another
	// Claude took the pane and got a new name): workers newer than every
	// other record in w1 and w3, so they would win both if they counted.
	write("wk_dead1.json", `{"agent":{"name":"wk_dead1","workspace_id":"w1","worktree":"`+worker+`"},"task":{"ref":"DEAD-1"},"launched_at":"2026-10-20T12:00:00"}`)
	write("wk_dead3.json", `{"agent":{"name":"wk_dead3","workspace_id":"w3","worktree":"`+worker+`"},"task":{"ref":"ESHOP-9#Z"},"launched_at":"2026-10-20T12:00:00"}`)

	oldDir, oldRecs := lineageDir, lineageRecords
	oldLive, oldLiveRead := liveAgents, liveAgentsRead
	lineageDir, lineageRecords = lin, nil
	liveAgents, liveAgentsRead = map[string]bool{
		"es-1270-f": true, "es-1270": true, "co_ab12": true, "wk_cd34": true, "wk_ef56": true,
	}, true
	t.Cleanup(func() {
		lineageDir, lineageRecords = oldDir, oldRecs
		liveAgents, liveAgentsRead = oldLive, oldLiveRead
	})

	if name, ref := harnessFor("w1", worker); name != "es-1270-f" || ref != "ESHOP-1270#F" {
		t.Errorf("worker by workspace: got %q %q", name, ref)
	}
	if name, ref := harnessFor("w2", taskDir); name != "es-1270" || ref != "ESHOP-1270" {
		t.Errorf("coordinator (non-git task dir): got %q %q", name, ref)
	}
	// Shared workspace: a worker beats the coordinator even when the
	// coordinator launched later; the latest worker wins among workers.
	if name, ref := harnessFor("w3", worker); name != "wk_ef56" || ref != "ESHOP-9#B" {
		t.Errorf("shared workspace: got %q %q", name, ref)
	}
	// The workspace match does not need the path: the agent's pane may have
	// moved to another directory since the record was written.
	if name, _ := harnessFor("w1", filepath.Join(dir, "elsewhere")); name != "es-1270-f" {
		t.Errorf("workspace match without path: got %q", name)
	}
	// A record naming a workspace never matches another workspace by path:
	// a new space in the same checkout must not inherit a leftover record.
	// Only the legacy record (no workspace_id) matches by path.
	if name, ref := harnessFor("w9", worker); name != "legacy" || ref != "LEGACY-1" {
		t.Errorf("other workspace, same checkout: got %q %q", name, ref)
	}
	if name, ref := harnessFor("w9", link); name != "legacy" || ref != "LEGACY-1" {
		t.Errorf("symlinked path: got %q %q", name, ref)
	}
	if name, ref := harnessFor("w9", filepath.Join(dir, "elsewhere")); name != "" || ref != "" {
		t.Errorf("no record: got %q %q", name, ref)
	}
	if name, _ := harnessFor("w9", ""); name != "" {
		t.Errorf("empty dir: got %q", name)
	}

	// herdr unreadable: no record can be told stale, so every record counts
	// and the dead ones (launched later) win their workspaces.
	lineageRecords, liveAgents = nil, nil
	if name, _ := harnessFor("w1", worker); name != "wk_dead1" {
		t.Errorf("herdr unreadable, w1: got %q", name)
	}
	if name, _ := harnessFor("w3", worker); name != "wk_dead3" {
		t.Errorf("herdr unreadable, w3: got %q", name)
	}
	// No live agent at all: every record naming a workspace is dropped; the
	// legacy record (no workspace) cannot be checked and still matches by
	// path.
	lineageRecords, liveAgents = nil, map[string]bool{}
	if name, _ := harnessFor("w9", worker); name != "legacy" {
		t.Errorf("legacy record with no live agents: got %q", name)
	}
	if name, _ := harnessFor("w1", filepath.Join(dir, "elsewhere")); name != "" {
		t.Errorf("w1 with no live agents: got %q", name)
	}
}
