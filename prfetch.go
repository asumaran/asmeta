package main

// The writer of the shared PR cache (prshare.go): one GitHub GraphQL query
// for every branch of every workspace. A branch's PR is looked up where PRs
// of that checkout are opened: the `upstream` remote when there is one (a
// fork), else `origin`; only PRs whose head repo is the checkout's origin
// count, so another fork's branch of the same name never matches. The newest
// open PR wins, else the newest one, as `gh pr view` picks.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	prQueryTimeout = 30 * time.Second
	// prChunk caps the branches of one query, to keep each query's cost
	// bounded; more branches are more queries in the same refresh.
	prChunk   = 100
	prBackoff = 5 * time.Minute
	// prLockWait is how long a refresh waits for another one to finish.
	prLockWait = 45 * time.Second
)

// prPair is one branch to look up: the checkout's origin (head), where its
// PRs are opened (base), the branch, and the workspaces on it.
type prPair struct {
	head, base, branch string
	ids                []string
}

// pairOf is the pair of a located workspace, false when it has none.
func pairOf(w *workspace) (prPair, bool) {
	if w.path == "" || prSkipBranch(w.branch) {
		return prPair{}, false
	}
	head := strings.ToLower(githubSlug(w.path))
	if head == "" {
		return prPair{}, false
	}
	base := head
	if u := gitOut(w.path, "remote", "get-url", "upstream"); u != "" {
		if s := githubSlugFromURL(u); s != "" {
			base = strings.ToLower(s)
		}
	}
	return prPair{head: head, base: base, branch: w.branch, ids: []string{w.id}}, true
}

// collectPairs are the deduplicated pairs of every workspace in list.
func collectPairs(list wsList) []prPair {
	byKey := map[string]*prPair{}
	var keys []string
	for _, ws := range list.Result.Workspaces {
		p, ok := pairOf(locate(list, ws.ID))
		if !ok {
			continue
		}
		k := p.head + "\x00" + p.base + "\x00" + p.branch
		if cur, ok := byKey[k]; ok {
			cur.ids = append(cur.ids, ws.ID)
			continue
		}
		byKey[k] = &p
		keys = append(keys, k)
	}
	out := make([]prPair, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}

type prNode struct {
	Number         int       `json:"number"`
	URL            string    `json:"url"`
	State          string    `json:"state"`
	IsDraft        bool      `json:"isDraft"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	BaseRefName    string    `json:"baseRefName"`
	HeadRefName    string    `json:"headRefName"`
	UpdatedAt      time.Time `json:"updatedAt"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
}

// prQuery builds one query for pairs: an alias per base repo (rN) and per
// branch in it (bN), every value passed as a variable.
func prQuery(pairs []prPair) (query string, vars map[string]string, alias map[string][2]string) {
	vars = map[string]string{}
	alias = map[string][2]string{} // "rN.bM" -> base, branch
	repos := map[string][]string{}
	var bases []string
	for _, p := range pairs {
		if _, ok := repos[p.base]; !ok {
			bases = append(bases, p.base)
		}
		if !contains(repos[p.base], p.branch) {
			repos[p.base] = append(repos[p.base], p.branch)
		}
	}
	sort.Strings(bases)
	var decl, body strings.Builder
	b := 0
	for r, base := range bases {
		owner, name, _ := strings.Cut(base, "/")
		vars[fmt.Sprintf("o%d", r)], vars[fmt.Sprintf("n%d", r)] = owner, name
		fmt.Fprintf(&decl, "$o%d: String!, $n%d: String!, ", r, r)
		fmt.Fprintf(&body, "  r%d: repository(owner: $o%d, name: $n%d) {\n", r, r, r)
		for _, branch := range repos[base] {
			vars[fmt.Sprintf("b%d", b)] = branch
			fmt.Fprintf(&decl, "$b%d: String!, ", b)
			fmt.Fprintf(&body, "    b%d: pullRequests(headRefName: $b%d, first: 10, states: [OPEN, MERGED, CLOSED], orderBy: {field: CREATED_AT, direction: DESC}) {\n"+
				"      nodes { number url state isDraft title body baseRefName headRefName updatedAt headRepository { nameWithOwner } }\n    }\n", b, b)
			alias[fmt.Sprintf("r%d.b%d", r, b)] = [2]string{base, branch}
			b++
		}
		body.WriteString("  }\n")
	}
	query = "query(" + strings.TrimSuffix(decl.String(), ", ") + ") {\n" + body.String() + "}"
	return query, vars, alias
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ghGraphQL runs one query. gh exits non-zero when the answer carries
// errors, but still prints the data it got, so stdout is returned either way.
var ghGraphQL = func(ctx context.Context, query string, vars map[string]string) (stdout []byte, stderr string, err error) {
	args := []string{"api", "graphql", "-f", "query=" + query}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-f", k+"="+vars[k])
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.Bytes(), errb.String(), err
}

// prResult is what one refresh learned.
type prResult struct {
	checks      map[string]sharedPRCheck // by head repo
	ok          bool                     // some repo answered
	rateLimited bool
}

// pick is the PR of pair among the nodes of its alias.
func pick(p prPair, nodes []prNode) *prNode {
	var first *prNode
	for i := range nodes {
		n := &nodes[i]
		if n.HeadRepository == nil || strings.ToLower(n.HeadRepository.NameWithOwner) != p.head {
			continue
		}
		if n.State == "OPEN" {
			return n
		}
		if first == nil {
			first = n
		}
	}
	return first
}

// fetchPRs looks every pair up, prChunk pairs per query. A repo whose alias
// came back with an error is left out of the result, so its cached entries
// stay as they were.
func fetchPRs(pairs []prPair) prResult {
	res := prResult{checks: map[string]sharedPRCheck{}}
	for start := 0; start < len(pairs); start += prChunk {
		chunk := pairs[start:min(start+prChunk, len(pairs))]
		query, vars, alias := prQuery(chunk)
		ctx, cancel := context.WithTimeout(context.Background(), prQueryTimeout)
		out, stderr, err := ghGraphQL(ctx, query, vars)
		cancel()
		var resp struct {
			Data   map[string]map[string]*struct{ Nodes []prNode } `json:"data"`
			Errors []struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		if jerr := json.Unmarshal(out, &resp); jerr != nil || resp.Data == nil {
			if strings.Contains(strings.ToLower(stderr), "rate limit") {
				res.rateLimited = true
			}
			logf("pr query failed: %s", orDefault(strings.TrimSpace(firstLineOf(stderr)), fmt.Sprint(err)))
			continue
		}
		for _, e := range resp.Errors {
			if e.Type == "RATE_LIMITED" {
				res.rateLimited = true
			}
		}
		for key, ab := range alias {
			r, b, _ := strings.Cut(key, ".")
			repo := resp.Data[r]
			if repo == nil {
				continue // this repo failed (gone, no access): keep what was cached
			}
			conn := repo[b]
			if conn == nil {
				continue
			}
			res.ok = true
			for _, p := range chunk {
				if p.base != ab[0] || p.branch != ab[1] {
					continue
				}
				c, ok := res.checks[p.head]
				if !ok {
					c = sharedPRCheck{Branches: map[string]string{}, Pulls: map[string]sharedPR{}}
					res.checks[p.head] = c
				}
				n := pick(p, conn.Nodes)
				if n == nil {
					c.Branches[p.branch] = ""
					continue
				}
				c.Branches[p.branch] = n.URL
				c.Pulls[n.URL] = sharedPR{
					Repo: p.base, Number: n.Number, Head: n.HeadRefName, Base: n.BaseRefName,
					State: sharedPRState(n.State, n.IsDraft), Title: n.Title, Body: cutRunes(n.Body, 1500),
					UpdatedAt: n.UpdatedAt,
				}
			}
		}
	}
	return res
}

func firstLineOf(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// prFacts is what a workspace shows of its PR, to tell which ones changed.
func prFacts(s sharedPRs, p prPair) string {
	pr, _, _ := s.branch(p.head, p.branch)
	if pr == nil {
		return ""
	}
	return fmt.Sprintf("%d\x00%s\x00%s", pr.Number, pr.State, pr.Title)
}

// refreshMode says when a trigger queries GitHub.
type refreshMode int

const (
	refreshIfStale refreshMode = iota // only when a branch is missing or older than the focus throttle
	refreshAlways                     // unless the backoff after a rate limit is on
	refreshForced                     // always
)

// refreshPRs brings prs.json up to date for every workspace of list and
// returns the workspaces whose PR changed. Requests that arrive while a
// refresh runs wait for it and use its answer instead of querying again.
func refreshPRs(list wsList, mode refreshMode) []string {
	requested := time.Now()
	pairs := collectPairs(list)
	if len(pairs) == 0 {
		return nil
	}
	if mode == refreshIfStale && !prsStale(loadSharedPRs(), pairs, requested) {
		return nil
	}
	unlock, err := lockSharedPRs(prLockWait)
	if err != nil {
		logf("%v", err)
		return nil
	}
	defer unlock()
	cur := loadSharedPRs()
	if cur.LastQueryStartedAt.After(requested) {
		return nil // another refresh queried after this request; it republished what changed
	}
	if mode != refreshForced && cur.BackoffUntil != nil && requested.Before(*cur.BackoffUntil) {
		return nil
	}
	before := map[string]string{}
	for _, p := range pairs {
		before[p.head+"\x00"+p.branch] = prFacts(cur, p)
	}
	cur.LastQueryStartedAt = time.Now()
	res := fetchPRs(pairs)
	now := time.Now()
	cur.merge(res.checks, now)
	switch {
	case res.rateLimited:
		b := now.Add(prBackoff)
		cur.BackoffUntil = &b
	case res.ok:
		cur.BackoffUntil = nil
	}
	if err := cur.save(); err != nil {
		logf("%v", err)
		return nil
	}
	var changed []string
	for _, p := range pairs {
		if prFacts(cur, p) != before[p.head+"\x00"+p.branch] {
			changed = append(changed, p.ids...)
		}
	}
	return changed
}

// prsStale: some pair was never checked, or not within the focus throttle.
func prsStale(s sharedPRs, pairs []prPair, now time.Time) bool {
	for _, p := range pairs {
		_, checked, at := s.branch(p.head, p.branch)
		if !checked || now.Sub(at) >= time.Duration(focusThrottle)*time.Second {
			return true
		}
	}
	return false
}
