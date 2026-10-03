package main

import (
	"encoding/json"
	"os"
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
