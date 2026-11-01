package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The expectations below were produced by the bash plugin this port
// replaces (sed, perl, jq, shasum), so the Go code is pinned to its output.

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"fix(eshop|seo): [ESHOP-2562], quitar euskera": "quitar euskera",
		"feat!: ESHOP-1 add x":                         "add x",
		"ESHOP-2567, remove basque":                    "remove basque",
		"Plain title":                                  "Plain title",
	}
	for in, want := range cases {
		if got := describe(in); got != want {
			t.Errorf("describe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanDescriptor(t *testing.T) {
	answer := "  \n> \"**Arreglar el checkout de tarjetas de crédito.**\"\nmás\n"
	if got := cleanDescriptor(answer, 24); got != `> "**Arreglar el` {
		t.Errorf("got %q", got)
	}
	if got := cleanDescriptor("\"Arreglar checkout.\"\n", 40); got != "Arreglar checkout" {
		t.Errorf("got %q", got)
	}
	if got := cleanDescriptor("Supercalifragilisticoespialidoso", 10); got != "Supercalif" {
		t.Errorf("one long word is cut: %q", got)
	}
	if got := cleanDescriptor(" \n\n", 10); got != "" {
		t.Errorf("blank answer: %q", got)
	}
}

func TestPlanAIKeyAndPrompt(t *testing.T) {
	t.Setenv("ASMETA_LANG", "")
	w := &workspace{id: "w1", path: "/x", linked: true, branch: "feat/x", prNum: "12",
		prTitle: "feat: añadir X", prBody: "body", jiraSummary: "Summary", jiraDesc: "Desc", ticket: "ESHOP-1"}
	p := planAI(w)
	if p.key != "9b2c6f4bdaff927d09750705774e6744db0e5fc632f93392300b32a81ae3acd8" {
		t.Errorf("key = %s", p.key)
	}
	if p.limit != 36 {
		t.Errorf("limit = %d, want 40 minus len(\"#12 \")", p.limit)
	}
	if !strings.HasPrefix(p.prompt, "Escribe un descriptor muy corto, en español,") ||
		!strings.Contains(p.prompt, "como máximo 36 caracteres") ||
		!strings.HasSuffix(p.prompt, "\nTicket ESHOP-1: Summary\nDescripción del ticket: Desc") {
		t.Errorf("prompt = %q", p.prompt)
	}

	t.Setenv("ASMETA_LANG", "English")
	q := planAI(w)
	if q.key == p.key || !strings.HasPrefix(q.prompt, "Write a very short descriptor, in English,") {
		t.Errorf("another language must change the key and the prompt: %s %q", q.key, q.prompt)
	}

	if planAI(&workspace{path: "/x", linked: false, prNum: "1"}).key != "" {
		t.Error("a main checkout gets no descriptor")
	}
}

func TestTokensForAndReportArgs(t *testing.T) {
	stateDir = t.TempDir()
	w := &workspace{id: "w1", label: "eshop-2562", path: "/x", linked: true, branch: "feat/ESHOP-2562",
		ticket: "ESHOP-2562", parent: "ESHOP-2500", prNum: "12", prLabel: "pr#12 draft", prState: "draft",
		prTitle: "fix(eshop): ESHOP-2562 quitar euskera", jiraSummary: "Quitar euskera",
		harness: "es-2562-a", harnessRef: "ESHOP-2562#A"}
	tokens := tokensFor(w, aiPlan{})
	want := map[string]string{
		"title": "quitar euskera", "desc": "quitar euskera", "ticket": "ESHOP-2562",
		"parent": "↳ ESHOP-2500", "pr": "pr#12 draft", "pr_state": "draft", "ref": "",
		"harness": "es-2562-a", "harness_ref": "ESHOP-2562#A",
	}
	for name, v := range want {
		if tokens[name] != v {
			t.Errorf("tokens[%q] = %q, want %q", name, tokens[name], v)
		}
	}
	args := strings.Join(reportArgs("w1", tokens), " ")
	// what publish built before the token set was a value of its own
	wantArgs := "workspace report-metadata w1 --source asumaran.asmeta" +
		" --token title=quitar euskera --token desc=quitar euskera --token ticket=ESHOP-2562" +
		" --token parent=↳ ESHOP-2500 --token pr=pr#12 draft --token pr_state=draft --clear-token ref" +
		" --token harness=es-2562-a --token harness_ref=ESHOP-2562#A --clear-token gone"
	if args != wantArgs {
		t.Errorf("args =\n%s\nwant\n%s", args, wantArgs)
	}

	parked := tokensFor(&workspace{id: "w4", label: "martech-dashboard", path: "/x", linked: false,
		branch: "claude/devbox-readme", prNum: "6422", prLabel: "pr#6422 merged", prState: "merged"}, aiPlan{})
	if parked["pr"] != "" || parked["pr_state"] != "" || parked["title"] != "martech-dashboard" {
		t.Errorf("a main checkout parked on a merged PR's branch shows no PR: %v", parked)
	}
	open := tokensFor(&workspace{id: "w5", label: "repo", path: "/x", linked: false,
		branch: "feat/x", prNum: "7", prLabel: "pr#7 draft", prState: "draft"}, aiPlan{})
	if open["pr"] != "pr#7 draft" || open["pr_state"] != "draft" {
		t.Errorf("a main checkout keeps an open or draft PR: %v", open)
	}
	linkedMerged := tokensFor(&workspace{id: "w6", label: "wt", path: "/x", linked: true,
		branch: "feat/y", prNum: "8", prLabel: "pr#8 merged", prState: "merged", prTitle: "feat: y"}, aiPlan{})
	if linkedMerged["pr"] != "pr#8 merged" || linkedMerged["pr_state"] != "merged" {
		t.Errorf("a linked worktree keeps its merged PR: %v", linkedMerged)
	}

	bare := tokensFor(&workspace{id: "w2", label: "scratch"}, aiPlan{})
	if got := strings.Join(reportArgs("w2", bare), " "); got != "workspace report-metadata w2 --source asumaran.asmeta"+
		" --token title=scratch --clear-token desc --clear-token ticket --clear-token parent"+
		" --clear-token pr --clear-token pr_state --clear-token ref"+
		" --clear-token harness --clear-token harness_ref --clear-token gone" {
		t.Errorf("no checkout: %s", got)
	}

	coord := tokensFor(&workspace{id: "w3", label: "task: ESHOP-1270",
		harness: "es-1270", harnessRef: "ESHOP-1270"}, aiPlan{})
	if coord["harness"] != "es-1270" || coord["harness_ref"] != "ESHOP-1270" || coord["title"] != "task: ESHOP-1270" {
		t.Errorf("coordinator without checkout: %v", coord)
	}
}

func TestIdentityOf(t *testing.T) {
	cases := []struct {
		w    *workspace
		want string
	}{
		{&workspace{path: "/repo", branch: "feat/x"}, "/repo\nfeat/x"},
		// No branch (a coordinator space, a non-git cwd): no trailing
		// newline, so it matches what readText gives back after writeText.
		{&workspace{cwd: "/work/KEY"}, "/work/KEY"},
		// A branch switch in the same checkout changes the identity.
		{&workspace{path: "/repo", branch: "main"}, "/repo\nmain"},
	}
	for _, c := range cases {
		if got := identityOf(c.w); got != c.want {
			t.Errorf("identityOf(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
	dir := t.TempDir()
	stateDir = dir
	w := &workspace{id: "wid", path: "/repo", branch: "feat/x"}
	os.MkdirAll(dir+"/identity", 0o755)
	writeText(identityFile(w.id), identityOf(w))
	if readText(identityFile(w.id)) != identityOf(w) {
		t.Error("a written identity must read back equal")
	}
	w.branch = "main"
	if readText(identityFile(w.id)) == identityOf(w) {
		t.Error("a branch switch must change the identity")
	}
}

func TestPublishedCacheRoundTrip(t *testing.T) {
	stateDir = t.TempDir()
	tokens := map[string]string{"title": "quitar euskera", "ticket": "ESHOP-2562"}
	writeJSONFile(publishedCache("w1"), tokens)
	var got map[string]string
	readJSONFile(publishedCache("w1"), &got)
	if got["title"] != tokens["title"] || got["ticket"] != tokens["ticket"] {
		t.Errorf("got %v", got)
	}
	if strings.Join(reportArgs("w1", got), " ") != strings.Join(reportArgs("w1", tokens), " ") {
		t.Error("a replay must say what the publish said")
	}
}

func TestWorkspaceFromEnv(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "")
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"event":"x","data":{"workspace_id":"w9","previous":{"workspace_id":"w1"}}}`)
	if got := workspaceFromEnv(); got != "w9" {
		t.Errorf("got %q", got)
	}
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"z":{"workspace_id":"first"},"a":{"workspace_id":"second"}}`)
	if got := workspaceFromEnv(); got != "first" {
		t.Errorf("document order, not key order: %q", got)
	}
	t.Setenv("HERDR_WORKSPACE_ID", "env")
	if got := workspaceFromEnv(); got != "env" {
		t.Errorf("got %q", got)
	}
}

func TestADFText(t *testing.T) {
	doc := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hola"},{"text":"mundo","type":"text"}]},{"type":"paragraph","content":[{"type":"text","text":"fin"}]}]}`
	if got := strings.Join(parseOrdered([]byte(doc)).stringsUnder("text"), " "); got != "Hola mundo fin" {
		t.Errorf("got %q", got)
	}
}

func TestRegroupPlan(t *testing.T) {
	data, err := os.ReadFile("testdata/regroup.json")
	if err != nil {
		t.Fatal(err)
	}
	var list wsList
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range regroupPlan(list) {
		got = append(got, m.key+"\t"+strings.Join(m.sorted, " ")+"\t"+m.anchor)
	}
	// what the bash plugin's jq program prints for the same list
	want := []string{"/r/front/.git\tc d b a\tother", "/r/lone/.git\th g\t"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The label asmeta gives a space it controls is given back when the space
// stops being its own (its pane moved to another checkout), and only then:
// a label the user typed since stays.
func TestMaybeRenameAndReleaseLabel(t *testing.T) {
	stateDir = t.TempDir()
	log := filepath.Join(t.TempDir(), "log")
	herdrScript(t, `echo "$@" >> `+log)
	renames := func() string {
		data, _ := os.ReadFile(log)
		os.Remove(log)
		return chomp(string(data))
	}

	w := &workspace{id: "wEV", label: "feat-92-ui-polish", path: "/wt/convokia/feat-92-ui-polish", linked: true}
	maybeRename(w, "Pulido de UI con shadcn/ui")
	if got := renames(); got != "workspace rename wEV Pulido de UI con shadcn/ui" {
		t.Errorf("a linked worktree takes its title as label: %q", got)
	}
	if w.label != "Pulido de UI con shadcn/ui" || readText(renamedFile("wEV")) != "Pulido de UI con shadcn/ui" {
		t.Errorf("the label set is remembered: label %q, file %q", w.label, readText(renamedFile("wEV")))
	}
	maybeRename(w, "Pulido de UI con shadcn/ui")
	if got := renames(); got != "" {
		t.Errorf("a label that already matches is left alone: %q", got)
	}
	releaseLabel(w)
	if got := renames(); got != "" {
		t.Errorf("a space still controlled keeps its label: %q", got)
	}

	// The pane cd'd to a main checkout: the space is that checkout's now.
	w.path, w.linked = "/Developer/asgotoissues", false
	releaseLabel(w)
	if got := renames(); got != "workspace rename wEV asgotoissues" {
		t.Errorf("a released space gets herdr's own label back: %q", got)
	}
	if w.label != "asgotoissues" || readText(renamedFile("wEV")) != "" {
		t.Errorf("after the release: label %q, file %q", w.label, readText(renamedFile("wEV")))
	}
	releaseLabel(w)
	if got := renames(); got != "" {
		t.Errorf("released once: %q", got)
	}

	// A label typed by hand since is the user's.
	hand := &workspace{id: "w2", label: "wt", path: "/wt/a", linked: true}
	maybeRename(hand, "a title")
	renames()
	hand.label, hand.path, hand.linked = "mine", "/Developer/b", false
	releaseLabel(hand)
	if got := renames(); got != "" || hand.label != "mine" || readText(renamedFile("w2")) != "" {
		t.Errorf("a hand-typed label stays: %q, label %q, file %q", got, hand.label, readText(renamedFile("w2")))
	}

	// A space asmeta never renamed is never touched, stale label or not.
	other := &workspace{id: "w3", label: "whatever", path: "/Developer/c"}
	releaseLabel(other)
	if got := renames(); got != "" {
		t.Errorf("never renamed, never released: %q", got)
	}

	// Herdr's own label is the folder; a space with nowhere to sit keeps its name.
	nowhere := &workspace{id: "w4", label: "t", linked: true}
	maybeRename(nowhere, "t")
	nowhere.linked = false
	releaseLabel(nowhere)
	if got := renames(); got != "" {
		t.Errorf("no checkout, no folder to name it after: %q", got)
	}
}
