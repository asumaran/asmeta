package main

// A coordinator space has no checkout: its pane sits in an aswork task
// directory, ~/.claude/work/<KEY>/. taskInfoFor reads that TASK.md's
// frontmatter (machine-written by the aswork plugin's aswork-status, fixed
// keys and plain scalars, so line patterns are enough), and taskTokens turns
// it into sidebar rows, so a coordinator space says which task it drives
// instead of showing a bare label.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type taskInfo struct {
	key, title, parent string
	rows               int
	repos              []string // distinct repo basenames, row order
}

var (
	reTaskKey    = regexp.MustCompile(`(?m)^key:[ \t]*(\S+)[ \t]*$`)
	reTaskTitle  = regexp.MustCompile(`(?m)^title:[ \t]*(.*?)[ \t]*$`)
	reTaskParent = regexp.MustCompile(`(?m)^parent:[ \t]*(\S+)[ \t]*$`)
	reTaskRow    = regexp.MustCompile(`(?m)^  - id:`)
	reTaskRepo   = regexp.MustCompile(`(?m)^    repo:[ \t]*(\S+)[ \t]*$`)
)

// taskInfoFor is the task whose directory is cwd, nil when cwd holds no
// aswork TASK.md (then the caller falls back to the bare label).
func taskInfoFor(cwd string) *taskInfo {
	if cwd == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(cwd, "TASK.md"))
	if err != nil || !strings.HasPrefix(string(data), "---\n") {
		return nil
	}
	text := string(data)
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil
	}
	front := text[4 : 4+end+1]
	m := reTaskKey.FindStringSubmatch(front)
	if m == nil {
		return nil
	}
	t := &taskInfo{key: m[1]}
	if m := reTaskTitle.FindStringSubmatch(front); m != nil {
		t.title = unquoteYAML(m[1])
	}
	if m := reTaskParent.FindStringSubmatch(front); m != nil && m[1] != "none" {
		t.parent = m[1]
	}
	t.rows = len(reTaskRow.FindAllString(front, -1))
	seen := map[string]bool{}
	for _, m := range reTaskRepo.FindAllStringSubmatch(front, -1) {
		if m[1] == "none" {
			continue
		}
		base := filepath.Base(m[1])
		if !seen[base] {
			seen[base] = true
			t.repos = append(t.repos, base)
		}
	}
	return t
}

// unquoteYAML undoes the double-quoted scalar aswork-status writes
// (title: "..." with \" and \\ escaped); an unquoted value passes through.
func unquoteYAML(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\\`, "\x00")
		s = strings.ReplaceAll(s, `\"`, `"`)
		s = strings.ReplaceAll(s, "\x00", `\`)
	}
	return s
}

// taskTokens is a coordinator space's token set: row 1 names the task
// ("coord · <title>"), row 2 carries the deliverables summary ($ref), the
// parent task and, when the key is a real ticket, the KEY as $ticket, styled
// and sorted like any other ticket row.
func taskTokens(w *workspace, t *taskInfo) map[string]string {
	title := orDefault(t.title, t.key)
	ref := "no rows yet"
	if t.rows == 1 {
		ref = "1 row"
	} else if t.rows > 1 {
		ref = fmt.Sprintf("%d rows", t.rows)
	}
	if len(t.repos) > 0 {
		ref += " · " + t.repos[0]
		if len(t.repos) > 1 {
			ref += fmt.Sprintf(" +%d", len(t.repos)-1)
		}
	}
	parent := ""
	if t.parent != "" {
		parent = "↳ " + t.parent
	}
	return map[string]string{
		"title": cutRunes("coord · "+title, 44), "desc": title,
		"ticket": ticketRe.FindString(t.key), "parent": parent, "ref": ref,
		"harness": w.harness, "harness_ref": w.harnessRef,
	}
}
