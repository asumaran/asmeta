// asmeta reports what each workspace is about as herdr Space sidebar tokens.
//
//	asmeta                 workspace from the herdr hook env (event / action)
//	asmeta <workspace_id>  one workspace
//	asmeta --all           every workspace
//	--force                ignore the focus throttle
//	--regen                drop the cached AI descriptor and generate it again
//
// Tokens: $title (row 1: "#PR descriptor" on a linked worktree, the workspace
// label elsewhere), $desc (the descriptor alone, what asgoto searches),
// $ticket, $parent, $pr ("#123 draft", kept for sorting), $pr_state (draft,
// merged or closed) and $ref (the branch, on the main checkout and on
// worktrees without a ticket, so row 2 always says something).
//
// Sources: branch name (ticket key), `gh pr view` (PR), Jira REST (parent,
// summary, description; stacks from ~/.claude/asdev.local.md, the same file
// asdev / asgotoissues read) and `claude -p` (Haiku) for the descriptor.
//
// The descriptor is stable: with a PR it depends on the PR and the ticket
// only, without one it is generated once from the ticket (or from the commits
// when there is no ticket either) and frozen until a PR appears. Until it
// exists, or when generation fails, it falls back to the PR title, the Jira
// summary, then the label. Every hook publishes the fallback first; AI
// generation goes through a queue drained by a single process at a time, so
// concurrent events never pile up model calls or herdr plugin command slots.
//
// After reporting, the linked worktrees of each repo are reordered in the
// sidebar (regroup.go). ASMETA_REGROUP=0 disables it. Focus and rename events
// never reorder.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// version is stamped at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

const source = "asumaran.asmeta"

var (
	stateDir      string
	focusThrottle int64
	jiraTTL       int64
	claudeBin     string
)

var ticketRe = regexp.MustCompile(`[A-Z][A-Z0-9]+-[0-9]+`)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "asmeta: "+format+"\n", args...)
}

func envInt(name string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil {
		return v
	}
	return def
}

// ---- small file helpers (the state is plain-text files, one per fact) ----

// fileAge is the whole seconds since path was last modified.
func fileAge(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return time.Now().Unix() - fi.ModTime().Unix(), true
}

// writeText writes s and a newline to path, atomically.
func writeText(path, s string) { writeFileAtomic(path, []byte(s+"\n")) }

// readText is the file without its trailing newlines (what $(cat f) gave).
func readText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return chomp(string(data))
}

// chomp drops every trailing newline, as a shell command substitution does.
func chomp(s string) string { return strings.TrimRight(s, "\n") }

// cutRunes keeps the first n characters of s.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// gitOut runs git in dir and returns its stdout without trailing newlines,
// "" on failure.
func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return chomp(string(out))
}

// ---- herdr ----

type wsEntry struct {
	ID       string            `json:"workspace_id"`
	Label    string            `json:"label"`
	Tokens   map[string]string `json:"tokens"`
	Worktree *struct {
		CheckoutPath string `json:"checkout_path"`
		IsLinked     bool   `json:"is_linked_worktree"`
		RepoKey      string `json:"repo_key"`
	} `json:"worktree"`
}

type wsList struct {
	Result struct {
		Workspaces []wsEntry `json:"workspaces"`
	} `json:"result"`
}

func workspaceList() (wsList, error) {
	var l wsList
	out, err := herdrRun("workspace", "list")
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(out, &l)
}

// firstPaneCwd is the cwd of the first pane of a workspace.
func firstPaneCwd(id string) string {
	out, err := herdrRun("pane", "list")
	if err != nil {
		return ""
	}
	var l struct {
		Result struct {
			Panes []struct {
				WorkspaceID string `json:"workspace_id"`
				Cwd         string `json:"cwd"`
			} `json:"panes"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &l) != nil {
		return ""
	}
	for _, p := range l.Result.Panes {
		if p.WorkspaceID == id && p.Cwd != "" {
			return p.Cwd
		}
	}
	return ""
}

// ---- what a workspace is ----

type workspace struct {
	id, label, path string
	linked          bool
	branch, ticket  string
	parent          string
	prNum, prLabel  string
	prState         string
	prTitle, prBody string
	prBase          string
	jiraSummary     string
	jiraDesc        string
}

// collect works out one workspace of a `workspace list`. Everything runs
// against the checkout with git -C: hooks start in the plugin's directory.
func collect(list wsList, id string) *workspace {
	w := &workspace{id: id}
	var row *wsEntry
	for i := range list.Result.Workspaces {
		if list.Result.Workspaces[i].ID == id {
			row = &list.Result.Workspaces[i]
			break
		}
	}
	if row == nil {
		return w
	}
	w.label = row.Label
	if row.Worktree != nil {
		w.path, w.linked = row.Worktree.CheckoutPath, row.Worktree.IsLinked
	}
	if w.path == "" {
		// herdr reported no worktree metadata (the space predates its git
		// discovery): the first pane's cwd still tells which checkout it is.
		if cwd := firstPaneCwd(id); cwd != "" {
			if top := gitOut(cwd, "rev-parse", "--show-toplevel"); top != "" {
				w.path = top
				gitDir := gitOut(top, "rev-parse", "--absolute-git-dir")
				if gitDir != "" && gitDir != commonDir(top) {
					w.linked = true
				}
			}
		}
	}
	if fi, err := os.Stat(w.path); w.path == "" || err != nil || !fi.IsDir() {
		w.path = ""
		return w
	}

	w.branch = gitOut(w.path, "branch", "--show-current")
	w.ticket = ticketRe.FindString(w.branch)
	switch w.branch {
	case "", "main", "master", "develop": // a PR with that head is someone else's release train
	default:
		readPR(w)
	}
	if w.ticket != "" {
		parent, kind := jiraParent(w.ticket)
		// An epic is a container, not a sibling group: only real parents are shown.
		if kind != "Epic" {
			w.parent = parent
		}
		jiraTextFresh(w.ticket)
		w.jiraSummary = readText(filepath.Join(stateDir, "summaries", w.ticket))
		w.jiraDesc = readText(filepath.Join(stateDir, "descriptions", w.ticket))
	}
	return w
}

// commonDir is the physical path of the checkout's common git dir.
func commonDir(top string) string {
	d := gitOut(top, "rev-parse", "--git-common-dir")
	if d == "" {
		return ""
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(top, d)
	}
	if real, err := filepath.EvalSymlinks(d); err == nil {
		return real
	}
	return ""
}

// readPR fills the PR fields with `gh pr view` run in the checkout.
func readPR(w *workspace) {
	cmd := exec.Command("gh", "pr", "view", "--json", "number,state,isDraft,title,body,baseRefName")
	cmd.Dir = w.path
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return
	}
	var pr struct {
		Number      json.Number `json:"number"`
		State       string      `json:"state"`
		IsDraft     bool        `json:"isDraft"`
		Title       *string     `json:"title"`
		Body        *string     `json:"body"`
		BaseRefName *string     `json:"baseRefName"`
	}
	if json.Unmarshal(out, &pr) != nil {
		return
	}
	w.prNum = pr.Number.String()
	switch {
	case pr.IsDraft:
		w.prState = "draft"
	case pr.State == "MERGED":
		w.prState = "merged"
	case pr.State == "CLOSED":
		w.prState = "closed"
	}
	if pr.BaseRefName != nil {
		w.prBase = chomp(*pr.BaseRefName)
	}
	if pr.Title != nil {
		w.prTitle = chomp(*pr.Title)
	}
	if pr.Body != nil {
		w.prBody = chomp(cutRunes(*pr.Body, 1500))
	}
	w.prLabel = "#" + w.prNum
	if w.prState != "" {
		w.prLabel += " " + w.prState
	}
}

// branchCommits are the subjects of the commits the branch adds over its base
// (the PR's, else the remote default branch); "" when the base cannot be
// resolved.
func branchCommits(w *workspace) string {
	base := "origin/HEAD"
	if w.prBase != "" {
		base = "origin/" + w.prBase
	}
	if exec.Command("git", "-C", w.path, "rev-parse", "--verify", "-q", base).Run() != nil {
		return ""
	}
	return gitOut(w.path, "log", "-20", "--format=%s", base+"..HEAD")
}

var (
	reCCPrefix   = regexp.MustCompile(`^[a-z]+(\([^)]*\))?!?:[[:space:]]*`)
	reTicketLead = regexp.MustCompile(`^\[?` + ticketRe.String() + `\]?[,:]?[[:space:]]*`)
)

// describe strips what the other tokens already say from a title: the
// Conventional Commits prefix ("fix(eshop|seo): ") and a leading ticket key
// ("ESHOP-2562 ", "ESHOP-2567, "). Line by line, as sed does.
func describe(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = reCCPrefix.ReplaceAllString(l, "")
		lines[i] = reTicketLead.ReplaceAllString(l, "")
	}
	return chomp(strings.Join(lines, "\n"))
}

// ---- publishing ----

// publish reports the workspace's tokens, with the cached descriptor of p.
func publish(w *workspace, p aiPlan) {
	if w.path == "" {
		// No checkout: just the name.
		herdrDo("workspace", "report-metadata", w.id, "--source", source, "--token", "title="+w.label,
			"--clear-token", "desc", "--clear-token", "ticket", "--clear-token", "parent", "--clear-token", "pr",
			"--clear-token", "pr_state", "--clear-token", "ref")
		return
	}
	var title, desc, ref string
	if w.linked {
		cached := ""
		if p.key != "" && aiCached(p.key) {
			cached = readText(aiCache(p.key))
		}
		for _, c := range []string{cached, describe(w.prTitle), describe(w.jiraSummary)} {
			if strings.ReplaceAll(c, " ", "") != "" {
				desc = c
				break
			}
		}
		title = orDefault(desc, w.label)
		if w.prNum != "" {
			title = "#" + w.prNum + " " + title
		}
		if w.ticket == "" {
			ref = w.branch
		}
	} else {
		title, ref = w.label, w.branch
	}
	args := []string{"workspace", "report-metadata", w.id, "--source", source, "--token", "title=" + title}
	tok := func(name, v string) {
		if v != "" {
			args = append(args, "--token", name+"="+v)
		} else {
			args = append(args, "--clear-token", name)
		}
	}
	tok("desc", desc)
	tok("ticket", w.ticket)
	if w.parent != "" {
		tok("parent", "↳ "+w.parent)
	} else {
		tok("parent", "")
	}
	tok("pr", w.prLabel)
	tok("pr_state", w.prState)
	tok("ref", ref)
	if err := herdrDo(args...); err != nil {
		logf("report-metadata failed for %s: %v", w.id, err)
		return
	}
	writeFileAtomic(filepath.Join(stateDir, "last", w.id), nil)
	logf("%s %s title=%s ticket=%s parent=%s pr=%s", w.id, orDefault(w.branch, "?"), title, w.ticket, w.parent, w.prLabel)
}

// ---- entry ----

// workspaceFromEnv is the workspace a hook or an action is about.
func workspaceFromEnv() string {
	if id := os.Getenv("HERDR_WORKSPACE_ID"); id != "" {
		return id
	}
	for _, name := range []string{"HERDR_PLUGIN_EVENT_JSON", "HERDR_PLUGIN_CONTEXT_JSON"} {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		if ids := parseOrdered([]byte(v)).stringsUnder("workspace_id"); len(ids) > 0 {
			return ids[0]
		}
		return ""
	}
	return ""
}

// loadSecrets reads ~/.secrets into the environment: hooks inherit the
// server's environment, which may predate edits to it. The file is shell, so
// a shell reads it.
func loadSecrets() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".secrets")
	if _, err := os.Stat(path); err != nil {
		return
	}
	out, err := exec.Command("bash", "-c", `set -a; source "$1" >/dev/null 2>&1; env -0`, "bash", path).Output()
	if err != nil {
		return
	}
	for _, kv := range strings.Split(string(out), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" && os.Getenv(k) != v {
			os.Setenv(k, v)
		}
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	all, force, regen := false, false, false
	var ids []string
	for _, arg := range argv {
		switch {
		case arg == "--all":
			all = true
		case arg == "--force":
			force = true
		case arg == "--regen":
			regen = true
		case arg == "-version" || arg == "--version":
			fmt.Println(version)
			return 0
		case strings.HasPrefix(arg, "-"):
			logf("unknown flag %s", arg)
			return 2
		default:
			ids = append(ids, arg)
		}
	}
	event := os.Getenv("HERDR_PLUGIN_EVENT")
	if event == "startup" {
		all = true
	}
	stateDir = stateDirFor("asmeta")
	focusThrottle = envInt("ASMETA_FOCUS_THROTTLE_SECONDS", 120)
	jiraTTL = envInt("ASMETA_SUMMARY_TTL_SECONDS", 21600)
	claudeBin = os.Getenv("ASMETA_CLAUDE_BIN")
	if claudeBin == "" {
		if p, err := exec.LookPath("claude"); err == nil {
			claudeBin = p
		} else if home, err := os.UserHomeDir(); err == nil {
			claudeBin = filepath.Join(home, ".local", "bin", "claude")
		}
	}
	os.MkdirAll(stateDir, 0o755)
	loadSecrets()
	defer releaseLock()

	list, err := workspaceList()
	if err != nil {
		logf("herdr workspace list failed")
		return 1
	}
	if all {
		for _, w := range list.Result.Workspaces {
			ids = append(ids, w.ID)
		}
	} else if len(ids) == 0 {
		id := workspaceFromEnv()
		if id == "" {
			logf("no workspace id (pass one or run from a herdr hook)")
			return 2
		}
		ids = []string{id}
		if event == "workspace.focused" && !force {
			if age, ok := fileAge(filepath.Join(stateDir, "last", id)); ok && age < focusThrottle {
				return 0
			}
		}
	}

	for _, id := range ids {
		w := collect(list, id)
		p := planAI(w)
		if p.key != "" && regen {
			os.Remove(aiCache(p.key))
			os.Remove(aiCache(p.key) + ".fail")
		}
		publish(w, p)
		if p.key != "" && !aiCached(p.key) && !aiCooling(p.key) {
			enqueue(id)
		}
	}
	switch event {
	case "workspace.focused", "workspace.renamed":
	default:
		regroup()
	}
	drainQueue()
	return 0
}
