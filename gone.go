package main

// A deleted worktree. herdr keeps a space's checkout_path after the worktree
// is removed (aswork's archive, `wt remove`, a plain `git worktree remove`),
// and with no checkout every source asmeta reads goes with it: no branch, no
// PR pair, no lineage record once the task is archived. Publishing what is
// left would wipe the space's second row. Instead the space keeps the last
// tokens it published while the checkout existed, plus $gone ("deleted") so
// the row says the worktree is no longer there.
//
// A field the kept set lacks (deleted before this existed, or while herdr
// was down) is filled from what outlives the checkout: the aswork task that
// names its branch (the ticket), the shared PR cache (the PR and its state)
// and the branch name itself (a Jira key). Recovery reads local files only,
// fills one field at a time and never replaces a kept value.
//
// A checkout that cannot be statted for a reason other than "it does not
// exist" (permissions, a volume that is not mounted) keeps its row too, but
// without the marker: it may well come back.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// goneLabel is the value of $gone on a deleted worktree's space.
const goneLabel = "deleted"

// goneTokens is the token set of a space whose linked checkout is gone (or
// unreachable, w.keep): the last one published, the fresh harness IDs when
// the lineage still names the space, what can be recovered for the fields
// still empty, and $gone when the checkout is really absent.
func goneTokens(w *workspace) map[string]string {
	var last map[string]string
	readJSONFile(publishedCache(w.id), &last)
	tokens := map[string]string{}
	for k, v := range last {
		if k != "gone" {
			tokens[k] = v
		}
	}
	if w.harness != "" {
		tokens["harness"] = w.harness
	}
	if w.harnessRef != "" {
		tokens["harness_ref"] = w.harnessRef
	}
	recoverGone(w, tokens)
	if tokens["title"] == "" {
		tokens["title"] = w.label
	}
	if w.gone {
		tokens["gone"] = goneLabel
	}
	return tokens
}

// recoverGone fills the ticket and the PR of tokens when they are missing.
func recoverGone(w *workspace, tokens map[string]string) {
	if tokens["ticket"] != "" && tokens["pr"] != "" {
		return
	}
	branch, ticket := taskBranchFor(w.repoRoot, w.gonePath)
	slug := ""
	if w.repoRoot != "" {
		slug = strings.ToLower(githubSlug(w.repoRoot))
	}
	prs := loadSharedPRs()
	if branch == "" && slug != "" {
		branch = cachedBranchFor(prs, slug, w.gonePath)
	}
	if tokens["ticket"] == "" {
		if ticket == "" {
			ticket = ticketRe.FindString(branch)
		}
		if ticket != "" {
			tokens["ticket"] = ticket
		}
	}
	if tokens["pr"] != "" || slug == "" || prSkipBranch(branch) {
		return
	}
	pr, _, _ := prs.branch(slug, branch)
	if pr == nil {
		return
	}
	label := "pr#" + strconv.Itoa(pr.Number)
	if pr.State != "open" {
		label += " " + pr.State
		tokens["pr_state"] = pr.State
	}
	tokens["pr"] = label
}

// worktreeDirFor is the directory name a branch's worktree gets: the
// canonical ~/wt/<repo>/<branch> with every "/" of the branch as "-".
func worktreeDirFor(branch string) string { return strings.ReplaceAll(branch, "/", "-") }

// cachedBranchFor is the branch of the shared PR cache, in the head repo
// slug, whose worktree directory is gonePath's; "" when none or several.
func cachedBranchFor(prs sharedPRs, slug, gonePath string) string {
	if gonePath == "" {
		return ""
	}
	dir := filepath.Base(gonePath)
	found := ""
	for b := range prs.Branches[slug] {
		if worktreeDirFor(b) == dir {
			if found != "" {
				return ""
			}
			found = b
		}
	}
	return found
}

// samePath says whether two paths name the same directory, comparing the
// resolved ones when both exist.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}

var reTaskBranch = regexp.MustCompile(`(?m)^    branch:[ \t]*(\S+)[ \t]*$`)

// taskBranchFor finds the aswork deliverable whose worktree gonePath was (its
// repo is the same directory as repoRoot, its branch's directory gonePath's)
// and returns the branch and the task's ticket: "issue#n" for a GitHub-issue
// task, else the Jira key of the task. A task dir outlives its archived
// worktrees. Only the frontmatter is read; when more than one row matches the
// answer is ambiguous and nothing is returned.
func taskBranchFor(repoRoot, gonePath string) (branch, ticket string) {
	if repoRoot == "" || gonePath == "" {
		return "", ""
	}
	dirs, err := os.ReadDir(workDir)
	if err != nil {
		return "", ""
	}
	dir := filepath.Base(gonePath)
	matches := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		front, ok := taskFrontmatter(filepath.Join(workDir, d.Name(), "TASK.md"))
		if !ok {
			continue
		}
		rows := reTaskRow.Split(front, -1)
		for _, row := range rows[1:] {
			r := reTaskRepo.FindStringSubmatch(row)
			b := reTaskBranch.FindStringSubmatch(row)
			if r == nil || b == nil || !samePath(r[1], repoRoot) || worktreeDirFor(b[1]) != dir {
				continue
			}
			matches++
			branch, ticket = b[1], taskTicket(front)
		}
	}
	if matches != 1 {
		return "", ""
	}
	return branch, ticket
}

// taskTicket is the ticket a task shows: "issue#n" for a GitHub-issue link,
// else the Jira key of the task's key; "" when it has neither.
func taskTicket(front string) string {
	if m := reTaskLink.FindStringSubmatch(front); m != nil {
		if g := reGHIssueLink.FindStringSubmatch(m[1]); g != nil {
			return "issue#" + g[1]
		}
	}
	if m := reTaskKey.FindStringSubmatch(front); m != nil {
		return ticketRe.FindString(m[1])
	}
	return ""
}
