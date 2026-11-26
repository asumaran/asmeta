package main

// What a space without a PR, a ticket summary or an aswork task is about, as
// its owner wrote it: the heading of the HANDOFF.md at the checkout's root
// (a dispatched worker starts from one), else the first thing typed into the
// first Claude Code session of that checkout. It feeds the descriptor only
// when no title exists, so a ticket always wins.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// intentMax is how much of a first prompt the descriptor gets.
const intentMax = 600

// claudeHomes lists the Claude Code config directories whose projects/ may
// hold the checkout's sessions: CLAUDE_CONFIG_DIR and every ~/.claude*
// (one per account). Tests replace it.
var claudeHomes = func() []string {
	var homes []string
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		homes = append(homes, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		more, _ := filepath.Glob(filepath.Join(home, ".claude*"))
		homes = append(homes, more...)
	}
	return uniq(homes)
}

var (
	reNotAlnum      = regexp.MustCompile(`[^A-Za-z0-9]`)
	reHandoffLead   = regexp.MustCompile(`(?i)^#+\s*(handoff\s*[:.-]?\s*)?`)
	reSpaces        = regexp.MustCompile(`\s+`)
	sessionScanMax  = 400 // lines read per session before giving up
	sessionLineHint = []byte(`"user"`)
)

// sessionDirName is the directory Claude Code keeps a checkout's sessions
// in, under projects/: the path with everything but letters and digits as -.
func sessionDirName(path string) string {
	return reNotAlnum.ReplaceAllString(path, "-")
}

// workIntent is the handoff heading of the checkout, else its first prompt.
func workIntent(checkout string) string {
	if checkout == "" {
		return ""
	}
	if h := handoffHeading(filepath.Join(checkout, "HANDOFF.md")); h != "" {
		return h
	}
	return firstPrompt(checkout)
}

// handoffHeading is the first markdown heading of the file, without the
// "HANDOFF:" lead the aswork handoffs carry.
func handoffHeading(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 0; n < 20 && sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			return cutRunes(strings.TrimSpace(reHandoffLead.ReplaceAllString(line, "")), intentMax)
		}
	}
	return ""
}

// firstPrompt is the first prompt of the checkout's earliest session, over
// every account, on one line; "" when it has no session.
func firstPrompt(checkout string) string {
	name := sessionDirName(checkout)
	best, bestAt := "", ""
	for _, home := range claudeHomes() {
		files, _ := filepath.Glob(filepath.Join(home, "projects", name, "*.jsonl"))
		for _, f := range files {
			text, at := sessionFirstPrompt(f)
			if text != "" && (bestAt == "" || at < bestAt) {
				best, bestAt = text, at
			}
		}
	}
	return best
}

// sessionFirstPrompt is the first thing the user typed in one session file
// and when. Tool results, slash command wrappers, interruptions and the
// stock "read HANDOFF.md" opener are not prompts.
func sessionFirstPrompt(path string) (text, at string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for n := 0; n < sessionScanMax; n++ {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytesContain(line, sessionLineHint) {
			var e struct {
				Type        string `json:"type"`
				IsMeta      bool   `json:"isMeta"`
				IsSidechain bool   `json:"isSidechain"`
				Timestamp   string `json:"timestamp"`
				Message     struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &e) == nil && e.Type == "user" && !e.IsMeta && !e.IsSidechain {
				if t := promptText(e.Message.Content); t != "" {
					return t, e.Timestamp
				}
			}
		}
		if err == io.EOF || err != nil {
			break
		}
	}
	return "", ""
}

func bytesContain(b, sub []byte) bool { return bytes.Contains(b, sub) }

// promptText is a user message's typed text on one line, "" when the
// message is not something the user typed about the work.
func promptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return ""
		}
		for _, p := range parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				s = p.Text
				break
			}
		}
	}
	s = strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "[Request interrupted") ||
		strings.HasPrefix(s, "Caveat:") || strings.Contains(cutRunes(s, 200), "HANDOFF.md") {
		return ""
	}
	return cutRunes(s, intentMax)
}
