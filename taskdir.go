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
	reTaskKey     = regexp.MustCompile(`(?m)^key:[ \t]*(\S+)[ \t]*$`)
	reTaskTitle   = regexp.MustCompile(`(?m)^title:[ \t]*(.*?)[ \t]*$`)
	reTaskParent  = regexp.MustCompile(`(?m)^parent:[ \t]*(\S+)[ \t]*$`)
	reTaskLink    = regexp.MustCompile(`(?m)^link:[ \t]*"?(\S+?)"?[ \t]*$`)
	reGHIssueLink = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/issues/(\d+)$`)
	reTaskRow     = regexp.MustCompile(`(?m)^  - id:`)
	reTaskRepo    = regexp.MustCompile(`(?m)^    repo:[ \t]*(\S+)[ \t]*$`)
	reTaskPR      = regexp.MustCompile(`(?m)^    pr:[ \t]*"?(\S+?)"?[ \t]*$`)
)

// taskDeliverable is one row of a task's deliverables, as far as the
// sidebar needs it: where its work lives and the PR the harness recorded.
type taskDeliverable struct {
	id, repo, branch, pr string
}

// deliverableForRef is the deliverable a harness ref names (KEY#ID), nil
// when the ref has no deliverable part, no task directory or no such row.
// A worker's record carries that ref; its PR is the deliverable's.
func deliverableForRef(ref string) *taskDeliverable {
	key, id, ok := strings.Cut(ref, "#")
	if !ok || key == "" || id == "" {
		return nil
	}
	front, ok := taskFrontmatter(filepath.Join(workDir, key, "TASK.md"))
	if !ok {
		return nil
	}
	for _, row := range reTaskRow.Split(front, -1)[1:] {
		first, _, _ := strings.Cut(row, "\n")
		if strings.TrimSpace(first) != id {
			continue
		}
		d := &taskDeliverable{id: id}
		if m := reTaskRepo.FindStringSubmatch(row); m != nil && m[1] != "none" {
			d.repo = m[1]
		}
		if m := reTaskBranch.FindStringSubmatch(row); m != nil && m[1] != "none" {
			d.branch = m[1]
		}
		if m := reTaskPR.FindStringSubmatch(row); m != nil && m[1] != "none" {
			d.pr = m[1]
		}
		return d
	}
	return nil
}

// taskFrontmatter is the frontmatter of the TASK.md at path, without the
// "---" fences; false when the file is unreadable or has none.
func taskFrontmatter(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "---\n") {
		return "", false
	}
	text := string(data)
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return "", false
	}
	return text[4 : 4+end+1], true
}

// taskInfoFor is the task whose directory is cwd, nil when cwd holds no
// aswork TASK.md (then the caller falls back to the bare label).
func taskInfoFor(cwd string) *taskInfo {
	if cwd == "" {
		return nil
	}
	front, ok := taskFrontmatter(filepath.Join(cwd, "TASK.md"))
	if !ok {
		return nil
	}
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

// issueTicketForRef is the "#n" shown by the task a harness ref points
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
		return "#" + m[1]
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
// title, row 2 carries the $ticket (Jira KEY, or "#<n>" for a GitHub
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
	// task's link ("#17"; the repo already rides in $harness_ref).
	ticket := ticketRe.FindString(t.key)
	if ticket == "" {
		if m := reGHIssueLink.FindStringSubmatch(t.link); m != nil {
			ticket = "#" + m[1]
		}
	}
	return map[string]string{
		"title": cutRunes(title, 44), "desc": title,
		"ticket": ticket, "parent": parent, "ref": ref,
		"harness": w.harness, "harness_ref": w.harnessRef,
	}
}
