package main

// The aswork harness identifies every agent with a worker name (es-1270-f)
// and a task ref (ESHOP-1270#F), recorded in one lineage file per agent.
// harnessFor maps a workspace's checkout (or cwd) to those two IDs so they
// can be published as the $harness / $harness_ref tokens.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// lineageDir is where the aswork harness keeps its lineage records, one JSON
// file per live agent (archived ones move to archive/). Tests point it at a
// fixture directory.
var lineageDir = defaultLineageDir()

func defaultLineageDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "agent-lineage")
}

type harnessID struct{ name, ref string }

// harnessByWorktree memoizes realpath(agent.worktree) → IDs for the whole
// run; one asmeta run reads the records at most once.
var harnessByWorktree map[string]harnessID

// harnessFor is the worker name and task ref of the lineage record whose
// agent.worktree is dir; both "" when no record matches (the tokens are then
// cleared on publish).
func harnessFor(dir string) (name, ref string) {
	if dir == "" {
		return "", ""
	}
	if harnessByWorktree == nil {
		harnessByWorktree = loadLineage()
	}
	id := harnessByWorktree[realPath(dir)]
	return id.name, id.ref
}

// loadLineage reads every top-level lineage record. The glob skips archive/
// and the .lock files by shape.
func loadLineage() map[string]harnessID {
	m := map[string]harnessID{}
	paths, _ := filepath.Glob(filepath.Join(lineageDir, "*.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec struct {
			Agent struct {
				Name     string `json:"name"`
				Worktree string `json:"worktree"`
			} `json:"agent"`
			Task struct {
				Ref string `json:"ref"`
			} `json:"task"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.Agent.Worktree == "" {
			continue
		}
		m[realPath(rec.Agent.Worktree)] = harnessID{rec.Agent.Name, rec.Task.Ref}
	}
	return m
}

// realPath resolves symlinks so both sides of a match compare physical paths
// (herdr reports /private/tmp where a record may say /tmp); a path that does
// not resolve is only cleaned.
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}
