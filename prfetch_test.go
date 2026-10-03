package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPRQuery(t *testing.T) {
	q, vars, alias := prQuery([]prPair{
		{head: "me/a", base: "me/a", branch: "feat/x"},
		{head: "me/a", base: "me/a", branch: `we"ird`},
		{head: "me/fork", base: "up/b", branch: "feat/x"},
	})
	if len(alias) != 3 || vars["o0"] != "me" || vars["n0"] != "a" || vars["o1"] != "up" || vars["n1"] != "b" || vars["b1"] != `we"ird` {
		t.Errorf("vars = %v alias = %v", vars, alias)
	}
	if strings.Contains(q, `we"ird`) || !strings.Contains(q, "$b2: String!") || !strings.Contains(q, "r1: repository(owner: $o1, name: $n1)") {
		t.Errorf("branch names travel as variables, never in the query:\n%s", q)
	}
}

// stubGraphQL replaces gh for one test and counts the queries.
func stubGraphQL(t *testing.T, stdout, stderr string) *int32 {
	t.Helper()
	var calls int32
	prev := ghGraphQL
	ghGraphQL = func(ctx context.Context, query string, vars map[string]string) ([]byte, string, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(stdout), stderr, nil
	}
	t.Cleanup(func() { ghGraphQL = prev })
	return &calls
}

const twoRepos = `{"data":{
 "r0":{
  "b0":{"nodes":[
   {"number":30,"url":"https://github.com/me/a/pull/30","state":"OPEN","title":"other fork","headRefName":"feat/x","headRepository":{"nameWithOwner":"them/a"}},
   {"number":20,"url":"https://github.com/me/a/pull/20","state":"MERGED","title":"old","headRefName":"feat/x","headRepository":{"nameWithOwner":"me/a"}},
   {"number":10,"url":"https://github.com/me/a/pull/10","state":"OPEN","isDraft":true,"title":"feat: x","body":"b","baseRefName":"main","headRefName":"feat/x","updatedAt":"2026-10-03T10:00:00Z","headRepository":{"nameWithOwner":"Me/A"}}]},
  "b1":{"nodes":[]}},
 "r1":null},
 "errors":[{"type":"NOT_FOUND","path":["r1"],"message":"Could not resolve to a Repository"}]}`

func TestFetchPRs(t *testing.T) {
	stubGraphQL(t, twoRepos, "")
	res := fetchPRs([]prPair{
		{head: "me/a", base: "me/a", branch: "feat/x"},
		{head: "me/a", base: "me/a", branch: "feat/none"},
		{head: "me/gone", base: "me/gone", branch: "feat/y"},
	})
	a := res.checks["me/a"]
	if !res.ok || a.Branches["feat/x"] != "https://github.com/me/a/pull/10" || a.Branches["feat/none"] != "" {
		t.Fatalf("checks = %+v", res.checks)
	}
	if _, ok := a.Branches["feat/none"]; !ok {
		t.Error("a branch with no PR is recorded as checked")
	}
	pr := a.Pulls["https://github.com/me/a/pull/10"]
	if pr.State != "draft" || pr.Number != 10 || pr.Repo != "me/a" || pr.Base != "main" || pr.UpdatedAt.IsZero() {
		t.Errorf("open PR of our head wins over the merged one and the other fork's: %+v", pr)
	}
	if _, ok := res.checks["me/gone"]; ok {
		t.Error("a repo that failed must be left out, so its cache stays")
	}
}

func TestFetchPRsRateLimited(t *testing.T) {
	stubGraphQL(t, "", "gh: API rate limit exceeded for user")
	res := fetchPRs([]prPair{{head: "me/a", base: "me/a", branch: "feat/x"}})
	if !res.rateLimited || res.ok || len(res.checks) != 0 {
		t.Errorf("res = %+v", res)
	}
}

// gitRepo makes a checkout on branch with origin at slug.
func gitRepo(t *testing.T, slug, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"remote", "add", "origin", "git@github.com:" + slug + ".git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestRefreshPRs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", filepath.Join(root, "plugins", "asumaran.asmeta"))
	stateDir, focusThrottle = stateDirFor("asmeta"), 120
	repo := gitRepo(t, "me/a", "feat/x")
	list := wsList{}
	list.Result.Workspaces = []wsEntry{{ID: "w1", Label: "a"}, {ID: "w2", Label: "dup"}}
	for i := range list.Result.Workspaces {
		list.Result.Workspaces[i].Worktree = &struct {
			CheckoutPath string `json:"checkout_path"`
			IsLinked     bool   `json:"is_linked_worktree"`
			RepoKey      string `json:"repo_key"`
		}{CheckoutPath: repo, IsLinked: true}
	}
	calls := stubGraphQL(t, twoRepos, "")

	changed := refreshPRs(list, refreshAlways)
	if *calls != 1 || strings.Join(changed, " ") != "w1 w2" {
		t.Fatalf("one query for both workspaces of the branch: calls=%d changed=%v", *calls, changed)
	}
	s := loadSharedPRs()
	if pr, _, _ := s.branch("me/a", "feat/x"); pr == nil || pr.Number != 10 {
		t.Fatalf("prs.json: %+v", s)
	}
	w := collect(list, "w1")
	if w.prLabel != "#10 draft" || w.prTitle != "feat: x" || w.prBase != "main" {
		t.Errorf("collect reads the cache: %+v", w)
	}

	if refreshPRs(list, refreshIfStale); *calls != 1 {
		t.Error("a fresh cache needs no query on focus")
	}
	if changed := refreshPRs(list, refreshAlways); *calls != 2 || len(changed) != 0 {
		t.Errorf("nothing changed, nothing to republish: calls=%d changed=%v", *calls, changed)
	}

	// another refresh started after this request: its answer is used
	s = loadSharedPRs()
	s.LastQueryStartedAt = time.Now().Add(time.Minute)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if refreshPRs(list, refreshForced); *calls != 2 {
		t.Error("a request covered by a later query must not query again")
	}

	// a rate limit backs off, except for a forced refresh
	s = loadSharedPRs()
	s.LastQueryStartedAt = time.Time{}
	s.save()
	stubGraphQL(t, "", "API rate limit exceeded")
	refreshPRs(list, refreshAlways)
	s = loadSharedPRs()
	if s.BackoffUntil == nil || s.BackoffUntil.Before(time.Now()) {
		t.Fatalf("backoff = %v", s.BackoffUntil)
	}
	if pr, _, _ := s.branch("me/a", "feat/x"); pr == nil {
		t.Error("a failed query must keep the cached PR")
	}
	calls = stubGraphQL(t, twoRepos, "")
	if refreshPRs(list, refreshAlways); *calls != 0 {
		t.Error("the backoff holds hook refreshes")
	}
	if refreshPRs(list, refreshForced); *calls != 1 {
		t.Error("a forced refresh ignores the backoff")
	}
	if s = loadSharedPRs(); s.BackoffUntil != nil {
		t.Error("a successful query clears the backoff")
	}
	os.Remove(sharedPRFile())
}

func TestUniq(t *testing.T) {
	if got := strings.Join(uniq([]string{"a", "b", "a", "c", "b"}), " "); got != "a b c" {
		t.Errorf("got %q", got)
	}
}
