package main

// A ticket's PR on a branch the space is not on. Some PRs are opened by
// someone else for the same ticket (an agent that names its branch
// coder/syf2-6373-..., while the space sits on SYF2-6373), so the space's
// own branch never has one. When a space with a ticket shows no PR (own
// branch, deliverable), the refresh searches the base repo for PRs that
// mention the ticket and keeps those whose head branch names it, from the
// space's origin. The match is stored in prs.json like any other branch, so
// from then on that branch is a pair of the space (ticketPair) and the
// regular query keeps it fresh; the search runs again only while nothing is
// found, at most once per ticketSearchEvery unless forced.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ticketSearchEvery = 15 * time.Minute
	// ticketChunk caps the searches of one query.
	ticketChunk = 20
)

// ticketBranchMatch says whether branch names ticket: case-insensitive, not
// glued to a letter or digit before it nor to a digit after it, so SYF2-637
// never matches syf2-6373.
func ticketBranchMatch(branch, ticket string) bool {
	if ticket == "" {
		return false
	}
	b, t := strings.ToLower(branch), strings.ToLower(ticket)
	for i := 0; i <= len(b)-len(t); {
		j := strings.Index(b[i:], t)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(t)
		if (j == 0 || !isAlnum(b[j-1])) && (end == len(b) || !isDigit(b[end])) {
			return true
		}
		i = j + 1
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// ticketPair is the pair of the cached branch, other than w's own, that
// names w's ticket and has a PR: an open one first, else the newest. false
// when there is none.
func ticketPair(w *workspace, prs sharedPRs) (prPair, bool) {
	if w.path == "" || w.ticket == "" {
		return prPair{}, false
	}
	head := strings.ToLower(githubSlug(w.path))
	if head == "" {
		return prPair{}, false
	}
	type cand struct {
		branch string
		pr     sharedPR
	}
	var cs []cand
	for b, e := range prs.Branches[head] {
		if b == w.branch || e.URL == nil || !ticketBranchMatch(b, w.ticket) {
			continue
		}
		if pr, ok := prs.Pulls[*e.URL]; ok {
			cs = append(cs, cand{b, pr})
		}
	}
	if len(cs) == 0 {
		return prPair{}, false
	}
	open := func(p sharedPR) bool { return p.State == "open" || p.State == "draft" }
	sort.Slice(cs, func(i, j int) bool {
		if open(cs[i].pr) != open(cs[j].pr) {
			return open(cs[i].pr)
		}
		if !cs[i].pr.UpdatedAt.Equal(cs[j].pr.UpdatedAt) {
			return cs[i].pr.UpdatedAt.After(cs[j].pr.UpdatedAt)
		}
		return cs[i].branch < cs[j].branch
	})
	return pairIn(w, cs[0].branch)
}

// ticketSearch is one ticket to look for in base, for the spaces in ids.
type ticketSearch struct {
	head, base, ticket string
	ids                []string
}

// ticketSearchFile marks when a ticket was last searched in head.
func ticketSearchFile(head, ticket string) string {
	return filepath.Join(stateDir, "ticketprs", strings.ReplaceAll(head, "/", "_")+"_"+ticket)
}

// ticketSearches are the searches the spaces in ws need: those with a
// ticket, on a branch that looks PRs up, still showing no PR in prs, whose
// ticket was not searched within ticketSearchEvery (unless forced).
func ticketSearches(ws []*workspace, prs sharedPRs, forced bool, now time.Time) []ticketSearch {
	byKey := map[string]*ticketSearch{}
	var keys []string
	for _, w0 := range ws {
		if w0.path == "" || w0.ticket == "" || prSkipBranch(w0.branch) {
			continue
		}
		w := *w0
		resolvePR(&w, prs)
		if w.prNum != "" {
			continue
		}
		p, ok := pairIn(&w, w.branch)
		if !ok {
			continue
		}
		k := p.head + "\x00" + p.base + "\x00" + w.ticket
		if cur, ok := byKey[k]; ok {
			cur.ids = append(cur.ids, w.id)
			continue
		}
		if !forced {
			if fi, err := os.Stat(ticketSearchFile(p.head, w.ticket)); err == nil && now.Sub(fi.ModTime()) < ticketSearchEvery {
				continue
			}
		}
		byKey[k] = &ticketSearch{head: p.head, base: p.base, ticket: w.ticket, ids: []string{w.id}}
		keys = append(keys, k)
	}
	out := make([]ticketSearch, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}

// ticketQuery builds one query with a search per ticket (sN), every value
// passed as a variable.
func ticketQuery(ss []ticketSearch) (query string, vars map[string]string) {
	vars = map[string]string{}
	var decl, body strings.Builder
	for i, s := range ss {
		vars[fmt.Sprintf("q%d", i)] = fmt.Sprintf("repo:%s is:pr %s", s.base, s.ticket)
		fmt.Fprintf(&decl, "$q%d: String!, ", i)
		fmt.Fprintf(&body, "  s%d: search(query: $q%d, type: ISSUE, first: 20) {\n"+
			"    nodes { ... on PullRequest { "+prNodeFields+" } }\n  }\n", i, i)
	}
	return "query(" + strings.TrimSuffix(decl.String(), ", ") + ") {\n" + body.String() + "}", vars
}

// ticketFound is what the searches found: the cache entries of each match,
// by head repo, and the spaces that now have a PR.
type ticketFound struct {
	checks map[string]sharedPRCheck
	ids    []string
}

// searchTicketPRs runs the searches, one query per account and chunk, as
// the PR query does (same map, tokens and backoff). Each account's outcome
// is folded into accounts, so a rate limit backs that account off. A search
// that answered is marked (ticketSearchFile) whether it found a PR or not.
func searchTicketPRs(ss []ticketSearch, backoff map[string]time.Time, accounts map[string]prAccountResult) ticketFound {
	found := ticketFound{checks: map[string]sharedPRCheck{}}
	if len(ss) == 0 {
		return found
	}
	orgs, def := loadGHAccounts()
	groups := map[string][]ticketSearch{}
	var order []string
	for _, s := range ss {
		org, _, _ := strings.Cut(s.base, "/")
		a := accountFor(orgs, def, org)
		if _, ok := groups[a]; !ok {
			order = append(order, a)
		}
		groups[a] = append(groups[a], s)
	}
	sort.Strings(order)
	for _, account := range order {
		if until, ok := backoff[account]; ok && time.Now().Before(until) {
			continue
		}
		token := ""
		if account != "" {
			tok, err := ghAuthToken(context.Background(), account)
			if err != nil {
				logf("ticket pr search: %v", err)
				continue
			}
			token = tok
		}
		ar := accounts[account]
		group := groups[account]
		for start := 0; start < len(group); start += ticketChunk {
			chunk := group[start:min(start+ticketChunk, len(group))]
			searchChunk(account, token, chunk, &ar, &found)
		}
		accounts[account] = ar
	}
	return found
}

func searchChunk(account, token string, chunk []ticketSearch, ar *prAccountResult, found *ticketFound) {
	query, vars := ticketQuery(chunk)
	ctx, cancel := context.WithTimeout(context.Background(), prQueryTimeout)
	out, stderr, err := ghGraphQL(ctx, query, vars, token)
	cancel()
	var resp struct {
		Data   map[string]*struct{ Nodes []prNode } `json:"data"`
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	if jerr := json.Unmarshal(out, &resp); jerr != nil || resp.Data == nil {
		if strings.Contains(strings.ToLower(stderr), "rate limit") {
			ar.rateLimited = true
		}
		logf("ticket pr search failed%s: %s", accountTag(account),
			orDefault(strings.TrimSpace(firstLineOf(stderr)), fmt.Sprint(err)))
		return
	}
	for _, e := range resp.Errors {
		if e.Type == "RATE_LIMITED" {
			ar.rateLimited = true
		}
	}
	for i, s := range chunk {
		conn := resp.Data[fmt.Sprintf("s%d", i)]
		if conn == nil {
			continue
		}
		ar.ok = true
		writeFileAtomic(ticketSearchFile(s.head, s.ticket), nil)
		var nodes []prNode
		for _, n := range conn.Nodes {
			if n.URL != "" && ticketBranchMatch(n.HeadRefName, s.ticket) {
				nodes = append(nodes, n)
			}
		}
		n := pick(prPair{head: s.head, base: s.base}, newestFirst(nodes))
		if n == nil {
			continue
		}
		c, ok := found.checks[s.head]
		if !ok {
			c = sharedPRCheck{Branches: map[string]string{}, Pulls: map[string]sharedPR{}}
			found.checks[s.head] = c
		}
		c.Branches[n.HeadRefName] = n.URL
		c.Pulls[n.URL] = sharedOf(s.base, n)
		found.ids = append(found.ids, s.ids...)
	}
}

// newestFirst orders search nodes as the pullRequests connection is
// (newest first), which a search by relevance is not; pick relies on it.
func newestFirst(nodes []prNode) []prNode {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].UpdatedAt.After(nodes[j].UpdatedAt) })
	return nodes
}
