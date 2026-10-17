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
	link               string // the task's ticket or issue URL, "" when none
	rows               int
	repos              []string // distinct repo basenames, row order
}

var (
	reTaskKey      = regexp.MustCompile(`(?m)^key:[ \t]*(\S+)[ \t]*$`)
	reTaskTitle    = regexp.MustCompile(`(?m)^title:[ \t]*(.*?)[ \t]*$`)
	reTaskParent   = regexp.MustCompile(`(?m)^parent:[ \t]*(\S+)[ \t]*$`)
	reTaskLink     = regexp.MustCompile(`(?m)^link:[ \t]*"?(\S+?)"?[ \t]*$`)
	reGHIssueLink  = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/issues/(\d+)$`)
	reTaskRow      = regexp.MustCompile(`(?m)^  - id:`)
	reTaskRepo     = regexp.MustCompile(`(?m)^    repo:[ \t]*(\S+)[ \t]*$`)
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
	if m := reTaskLink.FindStringSubmatch(front); m != nil {
		t.link = m[1]
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

// workDir is where the aswork harness keeps task state, one directory per
// task KEY (the stem of a worker's harness ref, e.g. "shopnest-badge#A").
var workDir = defaultWorkDir()

func defaultWorkDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "work")
}

// issueTicketForRef is the "Issue#n" shown by the task a harness ref points
// at, "" when the ref has no task directory or the task's link is no GitHub
// issue. It gives a worker the same $ticket as its coordinator: a GitHub
// issue number lives only in the task's link, never in a branch name.
func issueTicketForRef(ref string) string {
	key, _, _ := strings.Cut(ref, "#")
	if key == "" {
		return ""
	}
	t := taskInfoFor(filepath.Join(workDir, key))
	if t == nil {
		return ""
	}
	if m := reGHIssueLink.FindStringSubmatch(t.link); m != nil {
		return "Issue#" + m[1]
	}
	return ""
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

// taskTokens is a coordinator space's token set: row 1 is the bare task
// title, row 2 carries the $ticket (Jira KEY, or "Issue#<n>" for a GitHub
// issue), the deliverables summary ($ref) and the parent task. The
// coordinator mark is the co_* handle in $harness, not a title prefix.
func taskTokens(w *workspace, t *taskInfo) map[string]string {
	title := orDefault(t.title, t.key)
	ref := "no deliverables yet"
	if t.rows == 1 {
		ref = "1 deliverable"
	} else if t.rows > 1 {
		ref = fmt.Sprintf("%d deliverables", t.rows)
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
	// $ticket: a Jira KEY in the task key, else the GitHub issue from the
	// task's link ("Issue#17"; the repo already rides in $harness_ref).
	ticket := ticketRe.FindString(t.key)
	if ticket == "" {
		if m := reGHIssueLink.FindStringSubmatch(t.link); m != nil {
			ticket = "Issue#" + m[1]
		}
	}
	return map[string]string{
		"title": cutRunes(title, 44), "desc": title,
		"ticket": ticket, "parent": parent, "ref": ref,
		"harness": w.harness, "harness_ref": w.harnessRef,
	}
}
