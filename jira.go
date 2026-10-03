package main

// Jira: the parent, summary and description of a ticket, from the stacks of
// the asdev config (asdevconfig.go). The parent is cached forever (a ticket's
// parent does not change); the summary and the description are refreshed
// after jiraTTL. One plain-text file per ticket in parents/, summaries/ and
// descriptions/ of the state dir.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const jiraTimeout = 8 * time.Second

// jiraStacks are the stacks with a Jira site, in config order; none when the
// config is missing or does not parse.
func jiraStacks() []stack {
	all, err := readAsdevStacks(asdevConfigPath("ASDEV_CONFIG"))
	if err != nil {
		return nil
	}
	var out []stack
	for _, s := range all {
		if s.BaseURL != "" {
			out = append(out, s)
		}
	}
	return out
}

// jiraIssue is the issue JSON (parent, summary, description) from the first
// stack that has the ticket, the stack whose default project matches tried
// first; nil when no stack has it.
func jiraIssue(ticket string) []byte {
	project, _, _ := strings.Cut(ticket, "-")
	var preferred, rest []stack
	for _, s := range jiraStacks() {
		if s.DefaultProject == project {
			preferred = append(preferred, s)
		} else {
			rest = append(rest, s)
		}
	}
	client := &http.Client{Timeout: jiraTimeout}
	for _, s := range append(preferred, rest...) {
		cred, err := resolveCredential(s)
		if err != nil {
			continue
		}
		api := "3"
		if s.Type == "server" {
			api = "2"
		}
		req, err := http.NewRequest("GET", s.BaseURL+"/rest/api/"+api+"/issue/"+ticket+"?fields=parent,summary,description", nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		if cred.user != "" {
			req.SetBasicAuth(cred.user, cred.secret)
		} else {
			req.Header.Set("Authorization", "Bearer "+cred.secret)
		}
		resp, err := client.Do(req)
		if err != nil {
			logf("jira %s returned 000 for %s", s.Name, ticket)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return body
		case http.StatusNotFound: // not in this stack, try the next one
		default:
			logf("jira %s returned %d for %s", s.Name, resp.StatusCode, ticket)
		}
	}
	return nil
}

// jiraFetch fetches the ticket and caches its parent, summary and
// description (the text nodes of the Atlassian Document Format, or the plain
// string a server returns, cut to 1500 characters).
func jiraFetch(ticket string) {
	data := jiraIssue(ticket)
	if len(data) == 0 {
		return
	}
	root := parseOrdered(data)
	if root == nil {
		return
	}
	fields := root.get("fields")
	parent := fields.get("parent")
	parentKey := scalarString(parent.get("key"))
	parentType := scalarString(parent.get("fields").get("issuetype").get("name"))
	writeText(filepath.Join(stateDir, "parents", ticket), parentKey+"\t"+parentType)
	writeText(filepath.Join(stateDir, "summaries", ticket), scalarString(fields.get("summary")))
	desc := fields.get("description")
	var text string
	if s, ok := desc.scalar().(string); ok {
		text = s
	} else {
		text = strings.Join(desc.stringsUnder("text"), " ")
	}
	writeText(filepath.Join(stateDir, "descriptions", ticket), cutRunes(text, 1500))
}

func (n *jnode) scalar() any {
	if n == nil || n.isObj || n.isArr {
		return nil
	}
	return n.val
}

// scalarString is jq's `// ""` for a string field.
func scalarString(n *jnode) string {
	switch v := n.scalar().(type) {
	case string:
		return v
	case nil, bool:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// jiraParent is the ticket's parent key and the parent's issue type, fetched
// once.
func jiraParent(ticket string) (key, kind string) {
	path := filepath.Join(stateDir, "parents", ticket)
	if _, err := os.Stat(path); err != nil {
		jiraFetch(ticket)
	}
	key, kind, _ = strings.Cut(readText(path), "\t")
	return strings.TrimSpace(key), strings.TrimSpace(kind)
}

// jiraTextFresh refreshes the summary and description caches when either is
// missing or the summary is older than jiraTTL.
func jiraTextFresh(ticket string) {
	age, ok := fileAge(filepath.Join(stateDir, "summaries", ticket))
	if _, err := os.Stat(filepath.Join(stateDir, "descriptions", ticket)); !ok || age >= jiraTTL || err != nil {
		jiraFetch(ticket)
	}
}
