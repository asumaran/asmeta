package main

// The sidebar order: the linked worktrees of each repo sorted by group
// (parent ticket, or the ticket itself when there is no parent), then
// ticket, then PR number, so subtasks of one parent sit together right under
// the repo row. Ticket keys compare project first, number second. A repo
// already in order is left alone.

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// regroupMove is one workspace.move_block request: a repo whose linked
// worktrees are out of order.
type regroupMove struct {
	key    string   // the repo key
	sorted []string // its linked worktrees in the order they should have
	anchor string   // the workspace the block goes before; "" = the end
}

// sortKey mirrors the jq array the bash plugin sorted by: project and number
// of the group, project and number of the ticket, PR number, label. A nil
// project is jq's null, which sorts before every string.
type sortKey struct {
	groupProj  *string
	groupNum   int
	ticketProj *string
	ticketNum  int
	pr         int
	label      string
}

var rePRNum = regexp.MustCompile(`#([0-9]+)`)

// ticketProj is jq's `split("-")[0]`: null for the empty string.
func ticketProj(t string) *string {
	if t == "" {
		return nil
	}
	p, _, _ := strings.Cut(t, "-")
	return &p
}

// ticketNum is jq's `(split("-")[1] // "0") | tonumber? // 0`.
func ticketNum(t string) int {
	parts := strings.Split(t, "-")
	if t == "" || len(parts) < 2 {
		return 0
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0
	}
	return int(n)
}

func keyOf(w wsEntry) sortKey {
	parent := strings.TrimPrefix(w.Tokens["parent"], "↳ ")
	ticket := w.Tokens["ticket"]
	group := ticket
	if parent != "" {
		group = parent
	}
	pr := 0
	if m := rePRNum.FindStringSubmatch(w.Tokens["pr"]); m != nil {
		pr, _ = strconv.Atoi(m[1])
	}
	return sortKey{ticketProj(group), ticketNum(group), ticketProj(ticket), ticketNum(ticket), pr, w.Label}
}

func cmpProj(a, b *string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return strings.Compare(*a, *b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (a sortKey) less(b sortKey) bool {
	for _, c := range []int{
		cmpProj(a.groupProj, b.groupProj), cmpInt(a.groupNum, b.groupNum),
		cmpProj(a.ticketProj, b.ticketProj), cmpInt(a.ticketNum, b.ticketNum),
		cmpInt(a.pr, b.pr), strings.Compare(a.label, b.label),
	} {
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// regroupPlan lists the moves, repos in repo key order.
func regroupPlan(list wsList) []regroupMove {
	ws := list.Result.Workspaces
	order := make([]string, len(ws))
	for i, w := range ws {
		order[i] = w.ID
	}
	groups := map[string][]wsEntry{}
	var keys []string
	for _, w := range ws {
		if w.Worktree == nil || !w.Worktree.IsLinked {
			continue
		}
		k := w.Worktree.RepoKey
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], w)
	}
	sort.Strings(keys)
	var out []regroupMove
	for _, k := range keys {
		members := groups[k]
		current := make([]string, len(members))
		for i, m := range members {
			current[i] = m.ID
		}
		sortedMembers := append([]wsEntry(nil), members...)
		sort.SliceStable(sortedMembers, func(i, j int) bool { return keyOf(sortedMembers[i]).less(keyOf(sortedMembers[j])) })
		sorted := make([]string, len(sortedMembers))
		for i, m := range sortedMembers {
			sorted[i] = m.ID
		}
		if strings.Join(current, " ") == strings.Join(sorted, " ") {
			continue
		}
		lead := current[0]
		for _, w := range ws {
			if w.Worktree != nil && !w.Worktree.IsLinked && w.Worktree.RepoKey == k {
				lead = w.ID
				break
			}
		}
		in := map[string]bool{}
		for _, id := range current {
			in[id] = true
		}
		anchor := ""
		start := indexOf(order, lead)
		for _, id := range order[start+1:] {
			if !in[id] {
				anchor = id
				break
			}
		}
		out = append(out, regroupMove{k, sorted, anchor})
	}
	return out
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

// regroup moves every out-of-order block through the herdr socket.
func regroup() {
	if os.Getenv("ASMETA_REGROUP") != "" && os.Getenv("ASMETA_REGROUP") != "1" {
		return
	}
	sock := os.Getenv("HERDR_SOCKET_PATH")
	if fi, err := os.Stat(sock); sock == "" || err != nil || fi.Mode()&os.ModeSocket == 0 {
		logf("no herdr socket; skipping regroup")
		return
	}
	list, err := workspaceList()
	if err != nil {
		return
	}
	for _, m := range regroupPlan(list) {
		params := map[string]any{"workspace_ids": m.sorted}
		if m.anchor != "" {
			params["before_workspace_id"] = m.anchor
		}
		req, _ := json.Marshal(map[string]any{"id": "asmeta:regroup", "method": "workspace.move_block", "params": params})
		resp := socketCall(sock, req)
		var r struct {
			Result any `json:"result"`
		}
		if json.Unmarshal([]byte(resp), &r) == nil && r.Result != nil && r.Result != false {
			logf("regrouped %s: %s", filepath.Base(strings.TrimSuffix(m.key, "/.git")), strings.Join(m.sorted, " "))
		} else {
			logf("move_block failed for %s: %s", m.key, orDefault(resp, "no response"))
		}
	}
}

// socketCall sends one request line and returns the first line of the
// answer, "" when there is none within 5 seconds.
func socketCall(sock string, req []byte) string {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return ""
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimRight(line, "\n")
}
