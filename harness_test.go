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
	write("es-1270.json", `{"agent":{"name":"es-1270","workspace_id":"w2","worktree":"`+taskDir+`"},"task":{"ref":"ESHOP-1270"}}`)
	write("legacy.json", `{"agent":{"name":"legacy","worktree":"`+worker+`"},"task":{"ref":"LEGACY-1"}}`)
	write("broken.json", `{`)
	write("es-1270-f.lock", "")
	write(filepath.Join("archive", "old.json"), `{"agent":{"name":"old","workspace_id":"w1","worktree":"`+worker+`"},"task":{"ref":"OLD-1"}}`)

	oldDir, oldRecs := lineageDir, lineageRecords
	lineageDir, lineageRecords = lin, nil
	t.Cleanup(func() { lineageDir, lineageRecords = oldDir, oldRecs })

	if name, ref := harnessFor("w1", worker); name != "es-1270-f" || ref != "ESHOP-1270#F" {
		t.Errorf("worker by workspace: got %q %q", name, ref)
	}
	if name, ref := harnessFor("w2", taskDir); name != "es-1270" || ref != "ESHOP-1270" {
		t.Errorf("coordinator (non-git task dir): got %q %q", name, ref)
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
}
