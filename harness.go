package main

// The aswork harness identifies every agent with a worker name (es-1270-f)
// and a task ref (ESHOP-1270#F), recorded in one lineage file per agent.
// harnessFor maps a workspace to those two IDs so they can be published as
// the $harness / $harness_ref tokens. A record names the workspace its agent
// runs in (agent.workspace_id) and that match is exact: a leftover record
// never bleeds into another space sitting in the same checkout. Only a
// record without a workspace (an agent launched outside herdr) matches by
// checkout path.
//
// A record outlives its agent: nothing retires it when the pane closes or
// another Claude takes the pane over (the harness archives records on
// /aw:close, not on exit). So a record naming a workspace counts only while
// herdr still lists an agent with that name; herdr clears a name when its
// agent exits, so the live names are the truth. When herdr cannot be read
// every record counts, as before.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	launchedAt  string // ISO timestamp; survives relaunches
}

// lineageRecords memoizes the records for the whole run; one asmeta run
// reads them at most once. nil means not loaded yet.
var lineageRecords []lineageRecord

// liveAgents is the set of agent names herdr lists, read once per run with
// the records; nil when herdr could not be read (then no record is dropped).
// Tests set both before calling harnessFor.
var (
	liveAgents     map[string]bool
	liveAgentsRead bool
)

// harnessFor is the worker name and task ref of the lineage record whose
// agent sits in the workspace wsID (or, for a record that does not name a
// workspace, whose agent.worktree is dir); both "" when no record matches
// (the tokens are then cleared on publish). A coordinator may share the
// workspace with its workers (its tab lives in the task's initial
// worktree), so with several matches the pick is deterministic: workers
// (wk_ handles) beat coordinators, the most recently launched wins, and
// the name breaks a timestamp tie. Legacy records without a generated
// handle count as workers.
func harnessFor(wsID, dir string) (name, ref string) {
	if lineageRecords == nil {
		if !liveAgentsRead {
			liveAgents, liveAgentsRead = loadLiveAgents(), true
		}
		lineageRecords = loadLineage()
	}
	if r := pickRecord(func(r lineageRecord) bool {
		return r.workspaceID != "" && r.workspaceID == wsID
	}); r != nil {
		return r.name, r.ref
	}
	if dir == "" {
		return "", ""
	}
	real := realPath(dir)
	if r := pickRecord(func(r lineageRecord) bool {
		return r.workspaceID == "" && r.worktree != "" && r.worktree == real
	}); r != nil {
		return r.name, r.ref
	}
	return "", ""
}

// pickRecord selects among the records that match: non-coordinators first
// (anything not named co_*), then latest launchedAt, then lowest name.
func pickRecord(match func(lineageRecord) bool) *lineageRecord {
	var best *lineageRecord
	for i := range lineageRecords {
		r := &lineageRecords[i]
		if !match(*r) {
			continue
		}
		if best == nil || betterRecord(*r, *best) {
			best = r
		}
	}
	return best
}

func betterRecord(a, b lineageRecord) bool {
	aco, bco := strings.HasPrefix(a.name, "co_"), strings.HasPrefix(b.name, "co_")
	if aco != bco {
		return !aco
	}
	if a.launchedAt != b.launchedAt {
		return a.launchedAt > b.launchedAt
	}
	return a.name < b.name
}

// loadLiveAgents is the set of names of the agents herdr lists right now
// (unnamed agents add nothing); nil when herdr did not answer.
func loadLiveAgents() map[string]bool {
	out, err := herdrRun("agent", "list")
	if err != nil {
		return nil
	}
	var l struct {
		Result struct {
			Agents []struct {
				Name string `json:"name"`
			} `json:"agents"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &l) != nil {
		return nil
	}
	names := map[string]bool{}
	for _, a := range l.Result.Agents {
		if a.Name != "" {
			names[a.Name] = true
		}
	}
	return names
}

// loadLineage reads every top-level lineage record. The glob skips archive/
// and the .lock files by shape. A record naming a workspace whose agent
// herdr no longer lists is stale and skipped (see the file comment).
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
			LaunchedAt string `json:"launched_at"`
		}
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.Agent.WorkspaceID == "" && rec.Agent.Worktree == "" {
			continue
		}
		if rec.Agent.WorkspaceID != "" && liveAgents != nil && !liveAgents[rec.Agent.Name] {
			continue
		}
		worktree := ""
		if rec.Agent.Worktree != "" {
			worktree = realPath(rec.Agent.Worktree)
		}
		recs = append(recs, lineageRecord{
			name: rec.Agent.Name, ref: rec.Task.Ref,
			workspaceID: rec.Agent.WorkspaceID, worktree: worktree,
			launchedAt: rec.LaunchedAt,
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
