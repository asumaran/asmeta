package main

// The aswork harness identifies every agent with a worker name (es-1270-f)
// and a task ref (ESHOP-1270#F), recorded in one lineage file per agent.
// harnessFor maps a workspace to those two IDs so they can be published as
// the $harness / $harness_ref tokens. A record names the workspace its agent
// runs in (agent.workspace_id) and that match is exact: a leftover record
// never bleeds into another space sitting in the same checkout. Only a
// record without a workspace (an agent launched outside herdr) matches by
// checkout path.

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

type lineageRecord struct {
	name, ref   string
	workspaceID string
	worktree    string // realPath'd
}

// lineageRecords memoizes the records for the whole run; one asmeta run
// reads them at most once. nil means not loaded yet.
var lineageRecords []lineageRecord

// harnessFor is the worker name and task ref of the lineage record whose
// agent sits in the workspace wsID (or, for a record that does not name a
// workspace, whose agent.worktree is dir); both "" when no record matches
// (the tokens are then cleared on publish).
func harnessFor(wsID, dir string) (name, ref string) {
	if lineageRecords == nil {
		lineageRecords = loadLineage()
	}
	for _, r := range lineageRecords {
		if r.workspaceID != "" && r.workspaceID == wsID {
			return r.name, r.ref
		}
	}
	if dir == "" {
		return "", ""
	}
	real := realPath(dir)
	for _, r := range lineageRecords {
		if r.workspaceID == "" && r.worktree != "" && r.worktree == real {
			return r.name, r.ref
		}
	}
	return "", ""
}

// loadLineage reads every top-level lineage record. The glob skips archive/
// and the .lock files by shape.
func loadLineage() []lineageRecord {
	recs := []lineageRecord{}
	paths, _ := filepath.Glob(filepath.Join(lineageDir, "*.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec struct {
			Agent struct {
				Name        string `json:"name"`
				WorkspaceID string `json:"workspace_id"`
				Worktree    string `json:"worktree"`
			} `json:"agent"`
			Task struct {
				Ref string `json:"ref"`
			} `json:"task"`
		}
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.Agent.WorkspaceID == "" && rec.Agent.Worktree == "" {
			continue
		}
		worktree := ""
		if rec.Agent.Worktree != "" {
			worktree = realPath(rec.Agent.Worktree)
		}
		recs = append(recs, lineageRecord{
			name: rec.Agent.Name, ref: rec.Task.Ref,
			workspaceID: rec.Agent.WorkspaceID, worktree: worktree,
		})
	}
	return recs
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
