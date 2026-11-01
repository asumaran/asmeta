package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goneFix is an isolated asmeta world: a state dir next to a PR cache, an
// aswork work dir, a repo with a GitHub origin and a fake herdr that logs
// its arguments.
type goneFix struct {
	t     *testing.T
	root  string
	repo  string // the repo's main checkout, origin asumaran/convokia
	wt    string // the deleted checkout
	calls string // the fake herdr's log
}

func newGoneFix(t *testing.T) *goneFix {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "plugins", "asumaran.asmeta")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	oldState, oldWork := stateDir, workDir
	stateDir, workDir = state, filepath.Join(root, "work")
	t.Cleanup(func() { stateDir, workDir = oldState, oldWork })
	for _, d := range []string{"published", "identity"} {
		if err := os.MkdirAll(filepath.Join(state, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &goneFix{t: t, root: root, repo: gitRepo(t, "asumaran/convokia", "main"),
		wt: filepath.Join(root, "wt", "feat-44-fly"), calls: filepath.Join(root, "calls.log")}
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"pane list\" ]; then echo '{\"result\":{\"panes\":[]}}'; exit 0; fi\n" +
		"echo \"$@\" >> " + f.calls + "\n" +
		"[ -e " + filepath.Join(root, "fail") + " ] && exit 1\nexit 0\n"
	bin := filepath.Join(root, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_BIN_PATH", bin)
	return f
}

func (f *goneFix) published(id string, tokens map[string]string) {
	writeJSONFile(publishedCache(id), tokens)
}

func (f *goneFix) task(name, content string) {
	f.t.Helper()
	dir := filepath.Join(workDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// taskMDFor is a TASK.md with one deliverable row.
func taskMDFor(key, link, repo, branch string) string {
	return "---\nkey: " + key + "\ntitle: \"T\"\nlink: " + link + "\nparent: none\ndeliverables:\n" +
		"  - id: a\n    repo: " + repo + "\n    branch: " + branch + "\n    pr: none\n---\n\n## Goal\n"
}

// prs writes the shared PR cache with one branch of slug at the given state.
func (f *goneFix) prs(slug, branch string, number int, state string) {
	f.t.Helper()
	s := emptySharedPRs()
	url := "https://github.com/" + slug + "/pull/" + itoa(number)
	if s.Branches[slug] == nil {
		s.Branches[slug] = map[string]sharedBranch{}
	}
	s.Branches[slug][branch] = sharedBranch{URL: &url, CheckedAt: time.Now()}
	s.Pulls[url] = sharedPR{Repo: slug, Number: number, Head: branch, State: state, UpdatedAt: time.Now(), CheckedAt: time.Now()}
	data, _ := json.Marshal(s)
	if err := os.MkdirAll(filepath.Dir(sharedPRFile()), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(sharedPRFile(), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (f *goneFix) ws(id string) *workspace {
	return &workspace{id: id, label: "label " + id, linked: true, gone: true, gonePath: f.wt, repoRoot: f.repo}
}

func (f *goneFix) callLog() string {
	data, _ := os.ReadFile(f.calls)
	return string(data)
}

func TestGoneTokensKeepsThePublishedRow(t *testing.T) {
	f := newGoneFix(t)
	f.published("w1", map[string]string{"title": "Fly staging", "desc": "Fly staging", "ticket": "ESHOP-1",
		"pr": "pr#9 merged", "pr_state": "merged", "harness": "wk_old", "harness_ref": "T#A", "gone": "stale"})
	got := goneTokens(f.ws("w1"))
	want := map[string]string{"title": "Fly staging", "desc": "Fly staging", "ticket": "ESHOP-1",
		"pr": "pr#9 merged", "pr_state": "merged", "harness": "wk_old", "harness_ref": "T#A", "gone": "deleted"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// fresh lineage IDs win when non-empty; empty ones keep the saved ones
	w := f.ws("w1")
	w.harness, w.harnessRef = "wk_new", "T#B"
	if got := goneTokens(w); got["harness"] != "wk_new" || got["harness_ref"] != "T#B" {
		t.Errorf("fresh harness must win: %v", got)
	}
	w.harness, w.harnessRef = "", ""
	if got := goneTokens(w); got["harness"] != "wk_old" || got["harness_ref"] != "T#A" {
		t.Errorf("empty harness must keep the saved one: %v", got)
	}

	// nothing published: the label is the title
	if got := goneTokens(f.ws("w2")); got["title"] != "label w2" || got["gone"] != "deleted" {
		t.Errorf("nothing published: %v", got)
	}

	// keep: the row stays, the marker does not (and a stale one goes)
	k := f.ws("w1")
	k.gone, k.keep = false, true
	if got := goneTokens(k); got["gone"] != "" || got["ticket"] != "ESHOP-1" {
		t.Errorf("keep: %v", got)
	}
}

func TestGoneRecoveryFillsFieldByField(t *testing.T) {
	f := newGoneFix(t)
	f.task("convokia-44", taskMDFor("convokia-44", "https://github.com/asumaran/convokia/issues/44", f.repo, "feat/44-fly"))
	f.published("w1", map[string]string{"title": "Fly", "harness": "wk_a", "harness_ref": "convokia-44#A"})

	// the ticket is recovered, the PR is not in the cache yet
	got := goneTokens(f.ws("w1"))
	if got["ticket"] != "issue#44" || got["pr"] != "" || got["harness"] != "wk_a" {
		t.Fatalf("first run: %v", got)
	}
	f.published("w1", got)

	// a later run, with the PR now cached, completes it and keeps the ticket
	f.prs("asumaran/convokia", "feat/44-fly", 101, "merged")
	got = goneTokens(f.ws("w1"))
	if got["ticket"] != "issue#44" || got["pr"] != "pr#101 merged" || got["pr_state"] != "merged" {
		t.Fatalf("second run: %v", got)
	}

	// a kept value is never replaced
	f.published("w1", map[string]string{"title": "Fly", "ticket": "ESHOP-7", "pr": "pr#5"})
	got = goneTokens(f.ws("w1"))
	if got["ticket"] != "ESHOP-7" || got["pr"] != "pr#5" || got["pr_state"] != "" {
		t.Errorf("kept values must stay: %v", got)
	}
}

func TestTaskBranchFor(t *testing.T) {
	f := newGoneFix(t)
	other := filepath.Join(f.root, "elsewhere", "convokia") // same basename, another repo
	f.task("a", taskMDFor("a", "none", other, "feat/44-fly"))
	if b, tk := taskBranchFor(f.repo, f.wt); b != "" || tk != "" {
		t.Errorf("a repo with the same basename must not match: %q %q", b, tk)
	}

	f.task("b", taskMDFor("ESHOP-12", "none", f.repo, "feat/44-fly"))
	if b, tk := taskBranchFor(f.repo, f.wt); b != "feat/44-fly" || tk != "ESHOP-12" {
		t.Errorf("Jira key task: %q %q", b, tk)
	}
	// a symlinked spelling of the repo path is the same repo
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(f.repo, link); err != nil {
		t.Fatal(err)
	}
	if b, _ := taskBranchFor(link, f.wt); b != "feat/44-fly" {
		t.Errorf("symlinked repo path: %q", b)
	}

	f.task("b", taskMDFor("b", "https://github.com/asumaran/convokia/issues/44", f.repo, "feat/44-fly"))
	if _, tk := taskBranchFor(f.repo, f.wt); tk != "issue#44" {
		t.Errorf("issue link: %q", tk)
	}

	// a Jira link is no issue and the slug key has no ticket
	f.task("b", taskMDFor("b", "https://x.atlassian.net/browse/ABC-9", f.repo, "feat/44-fly"))
	if _, tk := taskBranchFor(f.repo, f.wt); tk != "" {
		t.Errorf("no ticket expected: %q", tk)
	}

	// a row faked in the Markdown body is ignored
	f.task("b", "---\nkey: b\nlink: none\ndeliverables: []\n---\n\n  - id: x\n    repo: "+f.repo+"\n    branch: feat/44-fly\n")
	if b, _ := taskBranchFor(f.repo, f.wt); b != "" {
		t.Errorf("a body row must be ignored: %q", b)
	}

	// two matching rows in one task, or in two tasks, are ambiguous
	row := "  - id: z\n    repo: " + f.repo + "\n    branch: feat/44-fly\n"
	f.task("b", "---\nkey: ESHOP-12\nlink: none\ndeliverables:\n"+row+row+"---\n")
	if b, tk := taskBranchFor(f.repo, f.wt); b != "" || tk != "" {
		t.Errorf("two rows: %q %q", b, tk)
	}
	f.task("b", taskMDFor("ESHOP-12", "none", f.repo, "feat/44-fly"))
	f.task("c", taskMDFor("ESHOP-13", "none", f.repo, "feat/44-fly"))
	if b, tk := taskBranchFor(f.repo, f.wt); b != "" || tk != "" {
		t.Errorf("two tasks: %q %q", b, tk)
	}

	if b, _ := taskBranchFor("", f.wt); b != "" {
		t.Error("empty repo root")
	}
	workDir = filepath.Join(f.root, "missing")
	if b, _ := taskBranchFor(f.repo, f.wt); b != "" {
		t.Error("missing work dir")
	}
}

func TestGoneRecoveryFromThePRCache(t *testing.T) {
	f := newGoneFix(t)
	f.wt = filepath.Join(f.root, "wt", "feat-ESHOP-77-thing")
	for state, want := range map[string]string{
		"open": "pr#7", "draft": "pr#7 draft", "merged": "pr#7 merged", "closed": "pr#7 closed",
	} {
		f.prs("asumaran/convokia", "feat/ESHOP-77-thing", 7, state)
		got := goneTokens(f.ws("w1"))
		if got["pr"] != want || got["ticket"] != "ESHOP-77" {
			t.Errorf("%s: %v", state, got)
		}
		wantState := state
		if state == "open" {
			wantState = ""
		}
		if got["pr_state"] != wantState {
			t.Errorf("%s: pr_state = %q", state, got["pr_state"])
		}
	}

	// two branches with the same directory name: nothing is guessed
	s := loadSharedPRs()
	s.Branches["asumaran/convokia"]["feat-ESHOP-77-thing"] = s.Branches["asumaran/convokia"]["feat/ESHOP-77-thing"]
	data, _ := json.Marshal(s)
	os.WriteFile(sharedPRFile(), data, 0o644)
	if got := goneTokens(f.ws("w1")); got["pr"] != "" || got["ticket"] != "" {
		t.Errorf("ambiguous branches: %v", got)
	}

	// no repo root, or a corrupt cache, recover nothing and do not break
	w := f.ws("w1")
	w.repoRoot = ""
	if got := goneTokens(w); got["pr"] != "" || got["gone"] != "deleted" {
		t.Errorf("no repo root: %v", got)
	}
	os.WriteFile(sharedPRFile(), []byte("{not json"), 0o644)
	if got := goneTokens(f.ws("w1")); got["pr"] != "" || got["gone"] != "deleted" {
		t.Errorf("corrupt cache: %v", got)
	}
}

func TestLocateGone(t *testing.T) {
	f := newGoneFix(t)
	locateOf := func(wt *wsWorktree) *workspace {
		list := wsList{}
		list.Result.Workspaces = []wsEntry{{ID: "w1", Label: "l", Worktree: wt}}
		return locate(list, "w1")
	}
	w := locateOf(&wsWorktree{CheckoutPath: f.wt, IsLinked: true, RepoRoot: f.repo})
	if !w.gone || w.keep || w.gonePath != f.wt || w.repoRoot != f.repo || w.path != "" {
		t.Errorf("linked and absent: %+v", w)
	}
	if w := locateOf(&wsWorktree{CheckoutPath: f.wt, IsLinked: false, RepoRoot: f.repo}); w.gone || w.keep {
		t.Errorf("a main checkout is not marked: %+v", w)
	}
	if w := locateOf(&wsWorktree{CheckoutPath: f.repo, IsLinked: true}); w.gone || w.keep || w.path != f.repo {
		t.Errorf("an existing checkout: %+v", w)
	}
	if w := locateOf(nil); w.gone || w.keep {
		t.Errorf("no worktree block: %+v", w)
	}

	// a stat failure other than ENOENT keeps the row without the marker
	locked := filepath.Join(f.root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	if _, err := os.Stat(filepath.Join(locked, "inner")); err == nil || os.IsNotExist(err) {
		t.Skip("this user can read through a mode 0 directory")
	}
	w = locateOf(&wsWorktree{CheckoutPath: filepath.Join(locked, "inner"), IsLinked: true, RepoRoot: f.repo})
	if w.gone || !w.keep {
		t.Errorf("unreadable: %+v", w)
	}
}

func TestGoneTransitionsAndPublish(t *testing.T) {
	f := newGoneFix(t)
	f.task("convokia-44", taskMDFor("convokia-44", "https://github.com/asumaran/convokia/issues/44", f.repo, "feat/44-fly"))
	f.prs("asumaran/convokia", "feat/44-fly", 101, "merged")
	f.published("w1", map[string]string{"title": "Fly"})

	// existing → deleted: the row is rebuilt and the marker set
	publish(f.ws("w1"), aiPlan{})
	log := f.callLog()
	for _, want := range []string{"--token title=Fly", "--token ticket=issue#44", "--token pr=pr#101 merged",
		"--token pr_state=merged", "--token gone=deleted"} {
		if !strings.Contains(log, want) {
			t.Errorf("report lacks %q:\n%s", want, log)
		}
	}
	var saved map[string]string
	readJSONFile(publishedCache("w1"), &saved)
	if saved["gone"] != "deleted" || saved["ticket"] != "issue#44" {
		t.Errorf("published cache: %v", saved)
	}

	// a server restart replays it
	os.Remove(f.calls)
	list := wsList{}
	list.Result.Workspaces = []wsEntry{{ID: "w1"}}
	replayPublished(list)
	if log := f.callLog(); !strings.Contains(log, "--token gone=deleted") || !strings.Contains(log, "--token pr=pr#101 merged") {
		t.Errorf("replay:\n%s", log)
	}

	// a second pass is stable
	os.Remove(f.calls)
	publish(f.ws("w1"), aiPlan{})
	first := f.callLog()
	os.Remove(f.calls)
	publish(f.ws("w1"), aiPlan{})
	if f.callLog() != first {
		t.Error("the report must be stable across runs")
	}

	// a failing report does not rewrite the cache
	os.WriteFile(filepath.Join(f.root, "fail"), nil, 0o644)
	f.published("w1", map[string]string{"title": "old"})
	publish(f.ws("w1"), aiPlan{})
	saved = nil
	readJSONFile(publishedCache("w1"), &saved)
	if saved["title"] != "old" || saved["gone"] != "" {
		t.Errorf("failed report rewrote the cache: %v", saved)
	}
	os.Remove(filepath.Join(f.root, "fail"))

	// restored: the checkout is back, the marker is cleared
	if out, err := exec.Command("git", "init", "-q", "-b", "feat/44-fly", f.wt).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	os.Remove(f.calls)
	list.Result.Workspaces = []wsEntry{{ID: "w1", Label: "Fly", Worktree: &wsWorktree{CheckoutPath: f.wt, IsLinked: true, RepoRoot: f.repo}}}
	w := locate(list, "w1")
	if w.gone || w.path != f.wt {
		t.Fatalf("restored checkout: %+v", w)
	}
	publish(w, aiPlan{})
	if log := f.callLog(); !strings.Contains(log, "--clear-token gone") || strings.Contains(log, "--token gone=") {
		t.Errorf("restored:\n%s", log)
	}
}
