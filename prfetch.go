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
	"os"
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

// ghGraphQL runs one query, as token's account when token is not empty (it
// overrides any ambient GH_TOKEN). gh exits non-zero when the answer carries
// errors, but still prints the data it got, so stdout is returned either way.
var ghGraphQL = func(ctx context.Context, query string, vars map[string]string, token string) (stdout []byte, stderr string, err error) {
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
	if token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.Bytes(), errb.String(), err
}

// prResult is what one refresh learned.
type prResult struct {
	checks map[string]sharedPRCheck // by head repo
	// accounts has an entry per account whose group actually queried;
	// groups skipped by a backoff or a missing token leave none.
	accounts map[string]prAccountResult
}

// prAccountResult is how one account's queries went.
type prAccountResult struct {
	ok          bool // some repo answered
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

// groupPairsByAccount splits pairs by the account that sees each pair's base
// org (the segment before "/"). Key "" is the group with no mapped account
// (no map, or no default): it queries as the active account, token-less.
// order is sorted so refreshes and tests are deterministic.
func groupPairsByAccount(pairs []prPair, orgs map[string]string, def string) (groups map[string][]prPair, order []string) {
	groups = map[string][]prPair{}
	for _, p := range pairs {
		org, _, _ := strings.Cut(p.base, "/")
		account := accountFor(orgs, def, org)
		if _, ok := groups[account]; !ok {
			order = append(order, account)
		}
		groups[account] = append(groups[account], p)
	}
	sort.Strings(order)
	return groups, order
}

// accountTag names an account in a log line; the token-less group has none.
func accountTag(account string) string {
	if account == "" {
		return ""
	}
	return fmt.Sprintf(" (account %s)", account)
}

// baseList is the deduplicated base repos of pairs, sorted, for log lines.
func baseList(pairs []prPair) string {
	seen := map[string]bool{}
	var bases []string
	for _, p := range pairs {
		if !seen[p.base] {
			seen[p.base] = true
			bases = append(bases, p.base)
		}
	}
	sort.Strings(bases)
	return strings.Join(bases, ", ")
}

// fetchPRs looks every pair up: pairs grouped by account (the shared
// org→account map), one token per account per refresh, prChunk pairs per
// query. A group whose account is in backoff or has no token is skipped with
// a log line; a skipped or failed repo is left out of the result, so its
// cached entries stay as they were.
func fetchPRs(pairs []prPair, backoff map[string]time.Time) prResult {
	res := prResult{checks: map[string]sharedPRCheck{}, accounts: map[string]prAccountResult{}}
	orgs, def := loadGHAccounts()
	groups, order := groupPairsByAccount(pairs, orgs, def)
	for _, account := range order {
		group := groups[account]
		if until, ok := backoff[account]; ok && time.Now().Before(until) {
			logf("pr refresh%s: rate-limit backoff until %s; skipped %s",
				accountTag(account), until.Format(time.RFC3339), baseList(group))
			continue
		}
		token := ""
		if account != "" {
			tok, err := ghAuthToken(context.Background(), account)
			if err != nil {
				logf("pr refresh: %v; skipped %s", err, baseList(group))
				continue
			}
			token = tok
		}
		res.accounts[account] = fetchGroup(account, token, group, res.checks)
	}
	return res
}

// fetchGroup runs one account's queries, accumulating into checks (a head
// repo seen by two groups accumulates branches, never replaces them).
func fetchGroup(account, token string, pairs []prPair, checks map[string]sharedPRCheck) prAccountResult {
	var ar prAccountResult
	noAccess := map[string]bool{}
	for start := 0; start < len(pairs); start += prChunk {
		chunk := pairs[start:min(start+prChunk, len(pairs))]
		query, vars, alias := prQuery(chunk)
		ctx, cancel := context.WithTimeout(context.Background(), prQueryTimeout)
		out, stderr, err := ghGraphQL(ctx, query, vars, token)
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
				ar.rateLimited = true
			}
			logf("pr query failed%s: %s", accountTag(account),
				orDefault(strings.TrimSpace(firstLineOf(stderr)), fmt.Sprint(err)))
			continue
		}
		for _, e := range resp.Errors {
			if e.Type == "RATE_LIMITED" {
				ar.rateLimited = true
			}
		}
		for key, ab := range alias {
			r, b, _ := strings.Cut(key, ".")
			repo := resp.Data[r]
			if repo == nil {
				// this repo failed (gone, no access): keep what was cached
				noAccess[ab[0]] = true
				continue
			}
			conn := repo[b]
			if conn == nil {
				continue
			}
			ar.ok = true
			for _, p := range chunk {
				if p.base != ab[0] || p.branch != ab[1] {
					continue
				}
				c, ok := checks[p.head]
				if !ok {
					c = sharedPRCheck{Branches: map[string]string{}, Pulls: map[string]sharedPR{}}
					checks[p.head] = c
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
	if len(noAccess) > 0 {
		var bases []string
		for b := range noAccess {
			bases = append(bases, b)
		}
		sort.Strings(bases)
		logf("pr refresh%s: no access to %s; cached entries kept",
			accountTag(account), strings.Join(bases, ", "))
	}
	return ar
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
	var backoff map[string]time.Time
	if mode != refreshForced {
		backoff = cur.AccountBackoff
	}
	before := map[string]string{}
	for _, p := range pairs {
		before[p.head+"\x00"+p.branch] = prFacts(cur, p)
	}
	queryStarted := time.Now()
	res := fetchPRs(pairs, backoff)
	if len(res.accounts) == 0 {
		return nil // every group skipped (backoff, no token): nothing queried
	}
	cur.LastQueryStartedAt = queryStarted
	now := time.Now()
	cur.merge(res.checks, now)
	for account, ar := range res.accounts {
		switch {
		case ar.rateLimited:
			if cur.AccountBackoff == nil {
				cur.AccountBackoff = map[string]time.Time{}
			}
			cur.AccountBackoff[account] = now.Add(prBackoff)
		case ar.ok:
			delete(cur.AccountBackoff, account)
		}
	}
	if len(cur.AccountBackoff) == 0 {
		cur.AccountBackoff = nil
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
