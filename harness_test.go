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
	write("es-1270-f.json", `{"agent":{"name":"es-1270-f","worktree":"`+worker+`"},"task":{"ref":"ESHOP-1270#F"}}`)
	write("es-1270.json", `{"agent":{"name":"es-1270","worktree":"`+taskDir+`"},"task":{"ref":"ESHOP-1270"}}`)
	write("broken.json", `{`)
	write("es-1270-f.lock", "")
	write(filepath.Join("archive", "old.json"), `{"agent":{"name":"old","worktree":"`+worker+`"},"task":{"ref":"OLD-1"}}`)

	oldDir, oldMap := lineageDir, harnessByWorktree
	lineageDir, harnessByWorktree = lin, nil
	t.Cleanup(func() { lineageDir, harnessByWorktree = oldDir, oldMap })

	if name, ref := harnessFor(worker); name != "es-1270-f" || ref != "ESHOP-1270#F" {
		t.Errorf("worker: got %q %q", name, ref)
	}
	if name, ref := harnessFor(taskDir); name != "es-1270" || ref != "ESHOP-1270" {
		t.Errorf("coordinator (non-git task dir): got %q %q", name, ref)
	}
	if name, ref := harnessFor(link); name != "es-1270-f" || ref != "ESHOP-1270#F" {
		t.Errorf("symlinked path: got %q %q", name, ref)
	}
	if name, ref := harnessFor(filepath.Join(dir, "elsewhere")); name != "" || ref != "" {
		t.Errorf("no record: got %q %q", name, ref)
	}
	if name, _ := harnessFor(""); name != "" {
		t.Errorf("empty dir: got %q", name)
	}
}
