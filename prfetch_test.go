package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// noGHMap points GH_ACCOUNTS_MAP at a path that does not exist, so no test
// reads the machine's real map.
func noGHMap(t *testing.T) {
	t.Helper()
	t.Setenv("GH_ACCOUNTS_MAP", filepath.Join(t.TempDir(), "absent"))
}

// ghMap writes a map fixture and points GH_ACCOUNTS_MAP at it.
func ghMap(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh-accounts")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ACCOUNTS_MAP", path)
}

// stubTokens replaces ghAuthToken with a table lookup and records the
// accounts asked.
func stubTokens(t *testing.T, tokens map[string]string) *[]string {
	t.Helper()
	var asked []string
	prev := ghAuthToken
	ghAuthToken = func(ctx context.Context, account string) (string, error) {
		asked = append(asked, account)
		token, ok := tokens[account]
		if !ok {
			return "", fmt.Errorf("gh auth token -u %s: no such account", account)
		}
		return token, nil
	}
	t.Cleanup(func() { ghAuthToken = prev })
	return &asked
}

// stubGraphQL replaces gh for one test with a fixed answer and counts the
// queries.
func stubGraphQL(t *testing.T, stdout, stderr string) *int32 {
	t.Helper()
	var calls int32
	prev := ghGraphQL
	ghGraphQL = func(ctx context.Context, query string, vars map[string]string, token string) ([]byte, string, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(stdout), stderr, nil
	}
	t.Cleanup(func() { ghGraphQL = prev })
	return &calls
}

// ghCall is one recorded ghGraphQL invocation.
type ghCall struct {
	token string
	bases []string
}

// ghStub answers each query from its own aliases and vars: repos maps a base
// repo to its branches' nodes (a JSON array). A base absent from repos
// answers null, as GitHub does for a repo the token cannot see. A base in
// limited rate-limits any call that includes it.
type ghStub struct {
	repos   map[string]map[string]string
	limited map[string]bool
	calls   []ghCall
}

var (
	stubRepoRe   = regexp.MustCompile(`(r\d+): repository\(owner: \$(o\d+), name: \$(n\d+)\)`)
	stubBranchRe = regexp.MustCompile(`(b\d+): pullRequests\(headRefName: \$(b\d+),`)
)

// stubGH installs s as ghGraphQL.
func stubGH(t *testing.T, s *ghStub) {
	t.Helper()
	prev := ghGraphQL
	ghGraphQL = func(ctx context.Context, query string, vars map[string]string, token string) ([]byte, string, error) {
		call := ghCall{token: token}
		data := map[string]any{}
		var cur map[string]any
		var branches map[string]string
		rateLimited := false
		for _, line := range strings.Split(query, "\n") {
			if m := stubRepoRe.FindStringSubmatch(line); m != nil {
				base := vars[m[2]] + "/" + vars[m[3]]
				call.bases = append(call.bases, base)
				if s.limited[base] {
					rateLimited = true
				}
				if bs, ok := s.repos[base]; ok {
					cur, branches = map[string]any{}, bs
					data[m[1]] = cur
				} else {
					cur, branches = nil, nil
					data[m[1]] = nil
				}
				continue
			}
			if m := stubBranchRe.FindStringSubmatch(line); m != nil && cur != nil {
				nodes := branches[vars[m[2]]]
				if nodes == "" {
					nodes = "[]"
				}
				cur[m[1]] = map[string]any{"nodes": json.RawMessage(nodes)}
			}
		}
		s.calls = append(s.calls, call)
		if rateLimited {
			return nil, "API rate limit exceeded", fmt.Errorf("exit status 1")
		}
		out, err := json.Marshal(map[string]any{"data": data})
		if err != nil {
			t.Error(err)
		}
		return out, "", nil
	}
	t.Cleanup(func() { ghGraphQL = prev })
}

const prTen = `[
 {"number":30,"url":"https://github.com/me/a/pull/30","state":"OPEN","title":"other fork","headRefName":"feat/x","headRepository":{"nameWithOwner":"them/a"}},
 {"number":20,"url":"https://github.com/me/a/pull/20","state":"MERGED","title":"old","headRefName":"feat/x","headRepository":{"nameWithOwner":"me/a"}},
 {"number":10,"url":"https://github.com/me/a/pull/10","state":"OPEN","isDraft":true,"title":"feat: x","body":"b","baseRefName":"main","headRefName":"feat/x","updatedAt":"2026-10-03T10:00:00Z","headRepository":{"nameWithOwner":"Me/A"}}]`

const prSeven = `[
 {"number":7,"url":"https://github.com/syftdata/b/pull/7","state":"OPEN","title":"feat: y","baseRefName":"main","headRefName":"feat/y","updatedAt":"2026-10-04T10:00:00Z","headRepository":{"nameWithOwner":"syftdata/b"}}]`

func TestFetchPRs(t *testing.T) {
	noGHMap(t)
	asked := stubTokens(t, nil)
	stubGH(t, &ghStub{repos: map[string]map[string]string{
		"me/a": {"feat/x": prTen, "feat/none": ""},
	}})
	done := captureLog(t)
	res := fetchPRs([]prPair{
		{head: "me/a", base: "me/a", branch: "feat/x"},
		{head: "me/a", base: "me/a", branch: "feat/none"},
		{head: "me/gone", base: "me/gone", branch: "feat/y"},
	}, nil)
	log := done()
	a := res.checks["me/a"]
	if !res.accounts[""].ok || a.Branches["feat/x"] != "https://github.com/me/a/pull/10" || a.Branches["feat/none"] != "" {
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
	if !strings.Contains(log, "no access to me/gone; cached entries kept") {
		t.Errorf("the inaccessible repo must be named in the log: %q", log)
	}
	if len(*asked) != 0 {
		t.Errorf("no map, no token lookups: %v", *asked)
	}
}

func TestFetchPRsRateLimited(t *testing.T) {
	noGHMap(t)
	stubTokens(t, nil)
	stubGraphQL(t, "", "gh: API rate limit exceeded for user")
	res := fetchPRs([]prPair{{head: "me/a", base: "me/a", branch: "feat/x"}}, nil)
	ar := res.accounts[""]
	if !ar.rateLimited || ar.ok || len(res.checks) != 0 {
		t.Errorf("res = %+v", res)
	}
}

func TestFetchPRsNoMap(t *testing.T) {
	noGHMap(t)
	asked := stubTokens(t, nil)
	stub := &ghStub{repos: map[string]map[string]string{
		"me/a":       {"feat/x": prTen},
		"syftdata/b": {"feat/y": prSeven},
	}}
	stubGH(t, stub)
	res := fetchPRs([]prPair{
		{head: "me/a", base: "me/a", branch: "feat/x"},
		{head: "syftdata/b", base: "syftdata/b", branch: "feat/y"},
	}, nil)
	if len(stub.calls) != 1 || stub.calls[0].token != "" {
		t.Fatalf("no map runs one token-less group: %+v", stub.calls)
	}
	if len(*asked) != 0 || len(res.checks) != 2 {
		t.Errorf("asked = %v checks = %+v", *asked, res.checks)
	}
}

func TestFetchPRsGroupsByAccount(t *testing.T) {
	ghMap(t, "syftdata alfredo-syft\ndefault asumaran\n")
	asked := stubTokens(t, map[string]string{"asumaran": "tokA", "alfredo-syft": "tokS"})
	stub := &ghStub{repos: map[string]map[string]string{
		"me/a":       {"feat/x": prTen},
		"syftdata/b": {"feat/y": prSeven},
	}}
	stubGH(t, stub)
	res := fetchPRs([]prPair{
		{head: "me/a", base: "me/a", branch: "feat/x"},
		{head: "syftdata/b", base: "syftdata/b", branch: "feat/y"},
	}, nil)
	if len(stub.calls) != 2 {
		t.Fatalf("one query per account: %+v", stub.calls)
	}
	// order is sorted by account: alfredo-syft, then asumaran
	if stub.calls[0].token != "tokS" || strings.Join(stub.calls[0].bases, " ") != "syftdata/b" ||
		stub.calls[1].token != "tokA" || strings.Join(stub.calls[1].bases, " ") != "me/a" {
		t.Errorf("each group queries with its account's token: %+v", stub.calls)
	}
	if strings.Join(*asked, " ") != "alfredo-syft asumaran" {
		t.Errorf("each account's token resolves once: %v", *asked)
	}
	if res.checks["me/a"].Branches["feat/x"] == "" || res.checks["syftdata/b"].Branches["feat/y"] == "" {
		t.Errorf("both groups merge into one result: %+v", res.checks)
	}
	if !res.accounts["asumaran"].ok || !res.accounts["alfredo-syft"].ok {
		t.Errorf("accounts = %+v", res.accounts)
	}
}

func TestFetchPRsSkipsUnresolvableAccount(t *testing.T) {
	ghMap(t, "syftdata alfredo-syft\ndefault asumaran\n")
	stubTokens(t, map[string]string{"asumaran": "tokA"})
	stub := &ghStub{repos: map[string]map[string]string{
		"me/a":       {"feat/x": prTen},
		"syftdata/b": {"feat/y": prSeven},
	}}
	stubGH(t, stub)
	done := captureLog(t)
	res := fetchPRs([]prPair{
		{head: "me/a", base: "me/a", branch: "feat/x"},
		{head: "syftdata/b", base: "syftdata/b", branch: "feat/y"},
	}, nil)
	log := done()
	if len(stub.calls) != 1 || stub.calls[0].token != "tokA" {
		t.Fatalf("only the resolvable group runs: %+v", stub.calls)
	}
	if _, ok := res.checks["syftdata/b"]; ok {
		t.Error("the skipped group's repos stay out of the result")
	}
	if _, ok := res.accounts["alfredo-syft"]; ok {
		t.Error("a skipped group reports nothing for its account")
	}
	if !res.accounts["asumaran"].ok {
		t.Errorf("accounts = %+v", res.accounts)
	}
	if !strings.Contains(log, "gh auth token -u alfredo-syft") || !strings.Contains(log, "skipped syftdata/b") {
		t.Errorf("the skipped account and repos must be named in the log: %q", log)
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
	ghMap(t, "syftdata alfredo-syft\ndefault asumaran\n")
	stubTokens(t, map[string]string{"asumaran": "tokA", "alfredo-syft": "tokS"})
	repo := gitRepo(t, "me/a", "feat/x")
	other := gitRepo(t, "syftdata/b", "feat/y")
	list := wsList{}
	list.Result.Workspaces = []wsEntry{{ID: "w1", Label: "a"}, {ID: "w2", Label: "dup"}, {ID: "w3", Label: "b"}}
	for i, dir := range []string{repo, repo, other} {
		list.Result.Workspaces[i].Worktree = &struct {
			CheckoutPath string `json:"checkout_path"`
			IsLinked     bool   `json:"is_linked_worktree"`
			RepoKey      string `json:"repo_key"`
		}{CheckoutPath: dir, IsLinked: true}
	}
	stub := &ghStub{repos: map[string]map[string]string{
		"me/a":       {"feat/x": prTen},
		"syftdata/b": {"feat/y": prSeven},
	}}
	stubGH(t, stub)

	changed := refreshPRs(list, refreshAlways)
	if len(stub.calls) != 2 || strings.Join(changed, " ") != "w1 w2 w3" {
		t.Fatalf("one query per account for every workspace: calls=%+v changed=%v", stub.calls, changed)
	}
	s := loadSharedPRs()
	if pr, _, _ := s.branch("me/a", "feat/x"); pr == nil || pr.Number != 10 {
		t.Fatalf("prs.json: %+v", s)
	}
	if pr, _, _ := s.branch("syftdata/b", "feat/y"); pr == nil || pr.Number != 7 {
		t.Fatalf("prs.json: %+v", s)
	}
	w := collect(list, "w1")
	if w.prLabel != "pr#10 draft" || w.prTitle != "feat: x" || w.prBase != "main" {
		t.Errorf("collect reads the cache: %+v", w)
	}

	if refreshPRs(list, refreshIfStale); len(stub.calls) != 2 {
		t.Error("a fresh cache needs no query on focus")
	}
	if changed := refreshPRs(list, refreshAlways); len(stub.calls) != 4 || len(changed) != 0 {
		t.Errorf("nothing changed, nothing to republish: calls=%d changed=%v", len(stub.calls), changed)
	}

	// another refresh started after this request: its answer is used
	s = loadSharedPRs()
	s.LastQueryStartedAt = time.Now().Add(time.Minute)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if refreshPRs(list, refreshForced); len(stub.calls) != 4 {
		t.Error("a request covered by a later query must not query again")
	}

	// a rate limit backs off only its own account
	s = loadSharedPRs()
	s.LastQueryStartedAt = time.Time{}
	s.save()
	stub.limited = map[string]bool{"syftdata/b": true}
	refreshPRs(list, refreshAlways) // 2 calls: syftdata's answers rate-limited
	s = loadSharedPRs()
	if until, ok := s.AccountBackoff["alfredo-syft"]; !ok || until.Before(time.Now()) {
		t.Fatalf("backoff = %v", s.AccountBackoff)
	}
	if _, ok := s.AccountBackoff["asumaran"]; ok {
		t.Error("the healthy account must not back off")
	}
	if pr, _, _ := s.branch("syftdata/b", "feat/y"); pr == nil {
		t.Error("a failed query must keep the cached PR")
	}
	stub.limited = nil
	if refreshPRs(list, refreshAlways); len(stub.calls) != 7 {
		t.Errorf("the backoff holds only its account's group: calls=%d", len(stub.calls))
	}
	if last := stub.calls[len(stub.calls)-1]; strings.Join(last.bases, " ") != "me/a" {
		t.Errorf("the backed-off group must be skipped: %+v", last)
	}
	if refreshPRs(list, refreshForced); len(stub.calls) != 9 {
		t.Errorf("a forced refresh ignores the backoff: calls=%d", len(stub.calls))
	}
	if s = loadSharedPRs(); s.AccountBackoff != nil {
		t.Errorf("a successful query clears its account's backoff: %v", s.AccountBackoff)
	}
	os.Remove(sharedPRFile())
}

func TestUniq(t *testing.T) {
	if got := strings.Join(uniq([]string{"a", "b", "a", "c", "b"}), " "); got != "a b c" {
		t.Errorf("got %q", got)
	}
}
