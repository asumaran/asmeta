package main

// The ticket a branch names. An upper-case KEY-123 is taken as written. Some
// branches carry it in lower case (an agent that names its branch
// coder/syf2-6278-..., cursor/syf2-6402-...), where the same shape also fits
// words that are no ticket (inbox-27, fix-123, node-18), so a lower-case
// match counts only when its project key is a known one.

import (
	"os"
	"regexp"
	"strings"
)

var (
	reAnyCaseTicket = regexp.MustCompile(`(?i)[a-z][a-z0-9]+-[0-9]+`)
	reTaskDirKey    = regexp.MustCompile(`^([A-Z][A-Z0-9]+)-[0-9]+`)
)

// branchTicket is the ticket branch names, upper-cased, "" when it names
// none.
func branchTicket(branch string) string {
	if t := ticketRe.FindString(branch); t != "" {
		return t
	}
	var keys map[string]bool
	for _, loc := range reAnyCaseTicket.FindAllStringIndex(branch, -1) {
		if loc[0] > 0 && isAlnum(branch[loc[0]-1]) {
			continue
		}
		if keys == nil {
			keys = projectKeys()
		}
		t := strings.ToUpper(branch[loc[0]:loc[1]])
		if project, _, _ := strings.Cut(t, "-"); keys[project] {
			return t
		}
	}
	return ""
}

// projectKeys are the project keys known on this machine: ASMETA_TICKET_KEYS
// (comma separated), every stack's default_project and the keys of the
// aswork tasks (a Linear team has no default_project, its tasks name it).
func projectKeys() map[string]bool {
	keys := map[string]bool{}
	for _, k := range strings.Split(os.Getenv("ASMETA_TICKET_KEYS"), ",") {
		if k = strings.ToUpper(strings.TrimSpace(k)); k != "" {
			keys[k] = true
		}
	}
	if stacks, err := readAsdevStacks(asdevConfigPath("ASDEV_CONFIG")); err == nil {
		for _, s := range stacks {
			if s.DefaultProject != "" {
				keys[strings.ToUpper(s.DefaultProject)] = true
			}
		}
	}
	if entries, err := os.ReadDir(workDir); err == nil {
		for _, e := range entries {
			if m := reTaskDirKey.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
				keys[m[1]] = true
			}
		}
	}
	return keys
}
