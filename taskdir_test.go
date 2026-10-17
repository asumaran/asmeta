package main

import (
	"os"
	"path/filepath"
	"testing"
)

const taskMD = `---
key: ESHOP-1270
title: "A\"B \\ headers"
link: "https://x/browse/ESHOP-1270"
parent: ESHOP-551
home: work
deliverables:
  - id: F
    repo: /Users/u/Developer/monorepo-front
    branch: feat/x
  - id: B
    repo: /Users/u/Developer/mm-monorepo
    branch: feat/y
  - id: K
    repo: /Users/u/Developer/mm-monorepo
    branch: feat/z
---

## Goal
`

func writeTask(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTaskInfoFor(t *testing.T) {
	ti := taskInfoFor(writeTask(t, taskMD))
	if ti == nil {
		t.Fatal("no task info")
	}
	if ti.key != "ESHOP-1270" || ti.title != `A"B \ headers` || ti.parent != "ESHOP-551" {
		t.Errorf("header: %+v", ti)
	}
	if ti.rows != 3 || len(ti.repos) != 2 || ti.repos[0] != "monorepo-front" || ti.repos[1] != "mm-monorepo" {
		t.Errorf("rows/repos: %+v", ti)
	}

	if taskInfoFor("") != nil || taskInfoFor(t.TempDir()) != nil {
		t.Error("missing TASK.md should give nil")
	}
	if taskInfoFor(writeTask(t, "---\ntitle: x\n---\n")) != nil {
		t.Error("frontmatter without key should give nil")
	}
}

func TestTokensForCoordinatorSpace(t *testing.T) {
	dir := writeTask(t, taskMD)
	w := &workspace{id: "w9", label: "es-1270", cwd: dir,
		harness: "es-1270", harnessRef: "ESHOP-1270"}
	tokens := tokensFor(w, aiPlan{})
	want := map[string]string{
		"title": `A"B \ headers`, "desc": `A"B \ headers`,
		"ticket": "ESHOP-1270", "parent": "↳ ESHOP-551",
		"ref": "3 deliverables · monorepo-front +1",
		"harness": "es-1270", "harness_ref": "ESHOP-1270",
	}
	for name, v := range want {
		if tokens[name] != v {
			t.Errorf("tokens[%q] = %q, want %q", name, tokens[name], v)
		}
	}

	// a slug task: no ticket token, singular row, title falls back to the key
	slug := writeTask(t, "---\nkey: scratch-prueba\ntitle: \"\"\nparent: none\ndeliverables:\n  - id: A\n    repo: none\n---\n")
	tokens = tokensFor(&workspace{id: "w8", label: "scratch-prueba", cwd: slug}, aiPlan{})
	if tokens["title"] != "scratch-prueba" || tokens["ticket"] != "" || tokens["ref"] != "1 deliverable" || tokens["parent"] != "" {
		t.Errorf("slug task: %v", tokens)
	}

	// a GitHub issue link becomes the $ticket when the key is no Jira KEY
	issue := writeTask(t, "---\nkey: shopnest-wish\ntitle: \"Wishlist\"\nlink: \"https://github.com/asumaran/shopnest/issues/18\"\nrepo: /Users/u/Developer/shopnest\nparent: none\ndeliverables:\n  - id: A\n    repo: /Users/u/Developer/other\n---\n")
	tokens = tokensFor(&workspace{id: "w7", label: "shopnest-wish", cwd: issue}, aiPlan{})
	if tokens["title"] != "Wishlist" || tokens["ticket"] != "issue#18" {
		t.Errorf("issue task: %v", tokens)
	}

	// a worker inherits its task's Issue#n through the harness ref: the
	// branch has no Jira KEY, so the worktree space asks the task dir
	oldWork := workDir
	workDir = t.TempDir()
	defer func() { workDir = oldWork }()
	taskDir := filepath.Join(workDir, "shopnest-wish")
	if err := os.Mkdir(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(issue, "TASK.md"))
	if err := os.WriteFile(filepath.Join(taskDir, "TASK.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	wk := &workspace{id: "w4", path: "/x", linked: true, label: "wk",
		harness: "wk_f3k2", harnessRef: "shopnest-wish#A"}
	tokens = tokensFor(wk, aiPlan{})
	if tokens["ticket"] != "issue#18" {
		t.Errorf("worker ticket: %v", tokens)
	}

	// a Jira link is not a GitHub issue: no fabricated ticket
	jlink := writeTask(t, "---\nkey: scratch-x\ntitle: \"X\"\nlink: \"https://x.atlassian.net/browse/ABC-9\"\nparent: none\ndeliverables: []\n---\n")
	tokens = tokensFor(&workspace{id: "w6", label: "scratch-x", cwd: jlink}, aiPlan{})
	if tokens["ticket"] != "" {
		t.Errorf("jira-linked slug task: %v", tokens)
	}
}
