package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTicketBranchMatch(t *testing.T) {
	for _, c := range []struct {
		branch, ticket string
		want           bool
	}{
		{"coder/syf2-6373-readonly-history-filter", "SYF2-6373", true},
		{"SYF2-6373", "SYF2-6373", true},
		{"feat/SYF2-6373", "SYF2-6373", true},
		{"coder/syf2-63730-x", "SYF2-6373", false},
		{"coder/xsyf2-6373", "SYF2-6373", false},
		{"coder/syf2-637-x", "SYF2-6373", false},
		{"coder/syf2-637-x syf2-6373", "SYF2-6373", true},
		{"feat/x", "", false},
	} {
		if got := ticketBranchMatch(c.branch, c.ticket); got != c.want {
			t.Errorf("ticketBranchMatch(%q, %q) = %v", c.branch, c.ticket, got)
		}
	}
}

const prCoder = `[
 {"number":6283,"url":"https://github.com/syftdata/m/pull/6283","state":"MERGED","title":"number match","baseRefName":"main","headRefName":"other","updatedAt":"2026-11-04T10:00:00Z","headRepository":{"nameWithOwner":"syftdata/m"}},
 {"number":40,"url":"https://github.com/syftdata/m/pull/40","state":"CLOSED","title":"old try","baseRefName":"main","headRefName":"coder/syf2-6373-old","updatedAt":"2026-11-01T10:00:00Z","headRepository":{"nameWithOwner":"syftdata/m"}},
 {"number":41,"url":"https://github.com/syftdata/m/pull/41","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","title":"fix: coder","baseRefName":"main","headRefName":"coder/syf2-6373-readonly","updatedAt":"2026-11-02T10:00:00Z","headRepository":{"nameWithOwner":"syftdata/m"}},
 {}]`

// TestTicketPRFallback: a space on the ticket branch, with no PR of its own,
// shows the PR someone else opened for the ticket on another branch.
func TestTicketPRFallback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", filepath.Join(root, "plugins", "asumaran.asmeta"))
	stateDir, focusThrottle = stateDirFor("asmeta"), 120
	noGHMap(t)
	stubTokens(t, nil)
	repo := gitRepo(t, "syftdata/m", "SYF2-6373")
	bare := gitRepo(t, "syftdata/m", "SYF2-6283")

	list := wsList{}
	list.Result.Workspaces = []wsEntry{{ID: "w1", Label: "a"}, {ID: "w2", Label: "b"}}
	list.Result.Workspaces[0].Worktree = &wsWorktree{CheckoutPath: repo, IsLinked: true, RepoRoot: repo}
	list.Result.Workspaces[1].Worktree = &wsWorktree{CheckoutPath: bare, IsLinked: true, RepoRoot: bare}

	stub := &ghStub{
		repos:    map[string]map[string]string{"syftdata/m": {}},
		searches: map[string]string{"repo:syftdata/m is:pr SYF2-6373": prCoder},
	}
	stubGH(t, stub)
	changed := refreshPRs(list, refreshAlways)
	if len(stub.calls) != 2 || strings.Join(stub.calls[1].searches, " | ") != "repo:syftdata/m is:pr SYF2-6373 | repo:syftdata/m is:pr SYF2-6283" {
		t.Fatalf("one search query for the spaces with no PR: %+v", stub.calls)
	}
	if strings.Join(changed, " ") != "w1" {
		t.Errorf("the space whose ticket PR was found is republished: %v", changed)
	}
	if w := collect(list, "w1"); w.prLabel != "pr#41 review" || w.prTitle != "fix: coder" {
		t.Errorf("the open PR whose branch names the ticket: %+v", w)
	}
	if w := collect(list, "w2"); w.prNum != "" {
		t.Errorf("a PR matching by number only is not the ticket's: %+v", w)
	}

	// the found branch is now a regular pair; the searches are throttled
	pairs := collectPairs(list)
	if len(pairs) != 3 || pairs[1].branch != "coder/syf2-6373-readonly" || pairs[1].ids[0] != "w1" {
		t.Fatalf("the ticket's branch joins the refresh: %+v", pairs)
	}
	stub.repos["syftdata/m"]["coder/syf2-6373-readonly"] = `[{"number":41,"url":"https://github.com/syftdata/m/pull/41","state":"MERGED","title":"fix: coder","baseRefName":"main","headRefName":"coder/syf2-6373-readonly","updatedAt":"2026-11-05T10:00:00Z","headRepository":{"nameWithOwner":"syftdata/m"}}]`
	changed = refreshPRs(list, refreshAlways)
	if len(stub.calls) != 3 {
		t.Errorf("w1 has its PR, w2 was searched recently: no search: %+v", stub.calls)
	}
	if strings.Join(changed, " ") != "w1" {
		t.Errorf("a change on the ticket's branch republishes the space: %v", changed)
	}
	if w := collect(list, "w1"); w.prLabel != "pr#41 merged" {
		t.Errorf("kept fresh by the regular query: %+v", w)
	}

	// forced: the space still without a PR is searched again
	if refreshPRs(list, refreshForced); len(stub.calls) != 5 || strings.Join(stub.calls[4].searches, " ") != "repo:syftdata/m is:pr SYF2-6283" {
		t.Errorf("a forced refresh searches again: %+v", stub.calls)
	}

	// its own PR wins
	stub.repos["syftdata/m"]["SYF2-6373"] = prOwnM
	refreshPRs(list, refreshForced)
	if w := collect(list, "w1"); w.prLabel != "pr#5" {
		t.Errorf("own branch first: %+v", w)
	}
	if _, err := os.Stat(ticketSearchFile("syftdata/m", "SYF2-6283")); err != nil {
		t.Errorf("an answered search is marked: %v", err)
	}
	os.Remove(sharedPRFile())
}

const prOwnM = `[
 {"number":5,"url":"https://github.com/syftdata/m/pull/5","state":"OPEN","title":"own","baseRefName":"main","headRefName":"SYF2-6373","updatedAt":"2026-10-30T10:00:00Z","headRepository":{"nameWithOwner":"syftdata/m"}}]`
