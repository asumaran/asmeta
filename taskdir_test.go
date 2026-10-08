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
		"title": `coord · A"B \ headers`, "desc": `A"B \ headers`,
		"ticket": "ESHOP-1270", "parent": "↳ ESHOP-551",
		"ref": "3 rows · monorepo-front +1",
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
	if tokens["title"] != "coord · scratch-prueba" || tokens["ticket"] != "" || tokens["ref"] != "1 row" || tokens["parent"] != "" {
		t.Errorf("slug task: %v", tokens)
	}
}
