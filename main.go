// asmeta reports what each workspace is about as herdr Space sidebar tokens.
//
//	asmeta                 workspace from the herdr hook env (event / action)
//	asmeta <workspace_id>  one workspace
//	asmeta --all           every workspace
//	--force                ignore the focus throttle
//	--regen                drop the cached AI descriptor and generate it again
//	--prs                  refresh the shared PR cache now and publish what changed
//
// Tokens: $title (row 1: "#PR descriptor" on a linked worktree, "coord ·
// <task title>" on an aswork coordinator space (taskdir.go), the workspace
// label elsewhere), $desc (the descriptor alone, what asgoto searches),
// $ticket, $parent, $pr ("#123 draft", kept for sorting), $pr_state (draft,
// merged or closed), $ref (the branch, on the main checkout and on
// worktrees without a ticket, so row 2 always says something), and $harness /
// $harness_ref (the aswork worker name and task ref from the lineage
// records, empty outside the harness).
//
// Sources: branch name (ticket key), the shared PR cache (prs.json, which this
// plugin writes with one GitHub query for every workspace: prfetch.go,
// prshare.go), Jira REST (parent,
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
// Every published token set is kept in published/<workspace id>.json. The
// startup hook replays those files before any network work, so the sidebar
// fills at once after a server restart (herdr does not persist tokens) and
// the refresh then corrects whatever changed while the server was down.
//
// After reporting, the linked worktrees of each repo are reordered in the
// sidebar (regroup.go). ASMETA_REGROUP=0 disables it. Focus and rename events
// never reorder.
package main

import (
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
	cwd             string
	linked          bool
	branch, ticket  string
	parent          string
	prNum, prLabel  string
	prState         string
	prTitle, prBody string
	prBase          string
	jiraSummary     string
	jiraDesc        string
	harness         string
	harnessRef      string
}

// collect works out one workspace of a `workspace list`: its checkout and
// branch (locate), its PR from the shared cache, its ticket from Jira.
func collect(list wsList, id string) *workspace {
	w := locate(list, id)
	w.harness, w.harnessRef = harnessFor(w.id, orDefault(w.path, w.cwd))
	if w.path == "" {
		return w
	}
	if pair, ok := pairOf(w); ok {
		readPR(w, loadSharedPRs(), pair)
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

// locate finds a workspace's checkout, branch and ticket. Everything runs
// against the checkout with git -C: hooks start in the plugin's directory.
func locate(list wsList, id string) *workspace {
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
		// The cwd is kept even outside git: a coordinator space sits in
		// ~/.claude/work/<KEY>, which is what its lineage record points at.
		if cwd := firstPaneCwd(id); cwd != "" {
			w.cwd = cwd
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

// readPR fills the PR fields from the shared cache (prfetch.go writes it).
func readPR(w *workspace, prs sharedPRs, pair prPair) {
	pr, _, _ := prs.branch(pair.head, pair.branch)
	if pr == nil {
		return
	}
	w.prNum = strconv.Itoa(pr.Number)
	if pr.State != "open" {
		w.prState = pr.State
	}
	w.prBase = chomp(pr.Base)
	w.prTitle = chomp(pr.Title)
	w.prBody = chomp(cutRunes(pr.Body, 1500))
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

// tokenNames is the order tokens are reported in. A token whose value is ""
// is cleared; title is always set.
var tokenNames = []string{"title", "desc", "ticket", "parent", "pr", "pr_state", "ref", "harness", "harness_ref"}

// tokensFor is the token set a workspace publishes, with the cached
// descriptor of p. A missing name reads as "".
func tokensFor(w *workspace, p aiPlan) map[string]string {
	if w.path == "" {
		// No checkout: a coordinator space (the pane sits in an aswork task
		// dir) renders the task; anything else keeps its name, plus the
		// harness IDs when a lineage record points at the cwd.
		if t := taskInfoFor(w.cwd); t != nil {
			return taskTokens(w, t)
		}
		return map[string]string{
			"title": w.label, "harness": w.harness, "harness_ref": w.harnessRef,
		}
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
	parent := ""
	if w.parent != "" {
		parent = "↳ " + w.parent
	}
	return map[string]string{
		"title": title, "desc": desc, "ticket": w.ticket, "parent": parent,
		"pr": w.prLabel, "pr_state": w.prState, "ref": ref,
		"harness": w.harness, "harness_ref": w.harnessRef,
	}
}

// reportArgs is the report-metadata command for a token set.
func reportArgs(id string, tokens map[string]string) []string {
	args := []string{"workspace", "report-metadata", id, "--source", source}
	for _, name := range tokenNames {
		if v := tokens[name]; v != "" || name == "title" {
			args = append(args, "--token", name+"="+v)
		} else {
			args = append(args, "--clear-token", name)
		}
	}
	return args
}

// publishedCache is where a workspace's last published token set lives.
func publishedCache(id string) string {
	return filepath.Join(stateDir, "published", id+".json")
}

// publish reports the workspace's tokens, with the cached descriptor of p,
// and keeps what it said on disk so the next server start replays it at once
// (herdr does not persist tokens across a restart).
func publish(w *workspace, p aiPlan) {
	tokens := tokensFor(w, p)
	if err := herdrDo(reportArgs(w.id, tokens)...); err != nil {
		logf("report-metadata failed for %s: %v", w.id, err)
		return
	}
	writeJSONFile(publishedCache(w.id), tokens)
	if w.path == "" {
		return
	}
	writeFileAtomic(filepath.Join(stateDir, "last", w.id), nil)
	logf("%s %s title=%s ticket=%s parent=%s pr=%s", w.id, orDefault(w.branch, "?"), tokens["title"], w.ticket, w.parent, w.prLabel)
}

// replayPublished reports each workspace's last published token set again,
// straight from disk, so the sidebar fills at once after a server restart
// while the slow refresh below (GitHub, Jira, git) recomputes everything.
// Stale-while-revalidate, as the rest of the family renders.
func replayPublished(list wsList) {
	n := 0
	for _, row := range list.Result.Workspaces {
		var tokens map[string]string
		readJSONFile(publishedCache(row.ID), &tokens)
		if len(tokens) == 0 {
			continue
		}
		if err := herdrDo(reportArgs(row.ID, tokens)...); err != nil {
			logf("replay failed for %s: %v", row.ID, err)
			continue
		}
		n++
	}
	if n > 0 {
		logf("replayed %d workspaces from the published cache", n)
	}
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
	all, force, regen, prsOnly := false, false, false, false
	var ids []string
	for _, arg := range argv {
		switch {
		case arg == "--all":
			all = true
		case arg == "--force":
			force = true
		case arg == "--regen":
			regen = true
		case arg == "--prs":
			prsOnly = true
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
	// On startup every token map is empty (herdr does not persist them), and
	// the refresh below takes seconds: replay the last published set first.
	if event == "startup" {
		replayPublished(list)
	}
	// The PR cache first: one query for every workspace, and every workspace
	// whose PR changed is published again, not only the one this run is for.
	mode := refreshAlways
	switch {
	case force || prsOnly:
		mode = refreshForced
	case event == "workspace.focused":
		mode = refreshIfStale
	}
	changed := refreshPRs(list, mode)

	switch {
	case prsOnly:
	case all:
		for _, w := range list.Result.Workspaces {
			ids = append(ids, w.ID)
		}
	case len(ids) == 0:
		id := workspaceFromEnv()
		if id == "" {
			logf("no workspace id (pass one or run from a herdr hook)")
			return 2
		}
		ids = []string{id}
		if event == "workspace.focused" && !force {
			if age, ok := fileAge(filepath.Join(stateDir, "last", id)); ok && age < focusThrottle {
				ids = nil
			}
		}
	}
	ids = uniq(append(ids, changed...))
	if len(ids) == 0 {
		return 0
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

// uniq keeps the first of each id, in order.
func uniq(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
