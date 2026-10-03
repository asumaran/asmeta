package main

// The AI descriptor: 3 to 5 words on what a linked worktree is about,
// generated with `claude -p` (Haiku) and cached under ai/<key>. What goes into
// the key is what may change the descriptor: the PR (or, without one, the
// ticket), so new commits never reword it. Generation goes through a queue
// (queue/<workspace id>) drained by one process at a time, so concurrent hook
// events never pile up model calls.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	aiModel        = "haiku"
	aiTimeout      = 90 * time.Second
	aiFailCooldown = 3600 // seconds
	// promptVersion: bump when the prompt changes, every cached descriptor
	// is regenerated.
	promptVersion = 1
	// titleMax is the room for the descriptor on row 1 (sidebar 44 columns,
	// minus the state icon).
	titleMax = 40
)

// aiPlan is what one workspace's descriptor depends on. key is empty when the
// workspace gets no descriptor.
type aiPlan struct {
	key    string
	limit  int
	prompt string
}

// aiLang is the language descriptors are written in: ASMETA_LANG, Spanish
// when unset.
func aiLang() (lang string, isDefault bool) {
	lang = strings.TrimSpace(os.Getenv("ASMETA_LANG"))
	switch strings.ToLower(lang) {
	case "", "es", "spanish", "español", "espanol":
		return "español", true
	}
	return lang, false
}

func planAI(w *workspace) aiPlan {
	p := aiPlan{limit: titleMax}
	if !w.linked || w.path == "" {
		return p
	}
	var basis, commits string
	switch {
	case w.prNum != "":
		basis = "pr\n" + w.prTitle + "\n" + w.prBody + "\n" + w.jiraSummary + "\n" + w.jiraDesc
		prefix := "#" + w.prNum + " "
		p.limit = titleMax - utf8.RuneCountInString(prefix)
	case w.ticket != "":
		basis = "ticket\n" + w.branch + "\n" + w.ticket + "\n" + w.jiraSummary + "\n" + w.jiraDesc
		commits = branchCommits(w)
	default:
		commits = branchCommits(w)
		// A branch name alone says nothing the label does not: wait for commits.
		if commits == "" {
			return p
		}
		basis = "branch\n" + w.branch + "\nhas_commits=1"
	}
	lang, isDefault := aiLang()
	if !isDefault {
		basis += "\nlang=" + lang
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\n%s\n%s\n", promptVersion, aiModel, basis)))
	p.key = hex.EncodeToString(sum[:])
	p.prompt = aiPrompt(w, p.limit, commits, lang, isDefault)
	return p
}

func aiPrompt(w *workspace, limit int, commits, lang string, isDefault bool) string {
	var b strings.Builder
	if isDefault {
		fmt.Fprintf(&b, "Escribe un descriptor muy corto, en español, de qué trata este trabajo de desarrollo. Se muestra en una lista de worktrees para reconocerlo de un vistazo.\n"+
			"Reglas: de 3 a 5 palabras y como máximo %d caracteres. Sin número de ticket ni de PR, sin prefijos como feat o fix, sin comillas ni punto final. Empieza con mayúscula. Responde solo con el descriptor.\n\n"+
			"Branch: %s", limit, w.branch)
		add := func(label, v string) {
			if v != "" {
				b.WriteString("\n" + label + v)
			}
		}
		add("Título del PR: ", w.prTitle)
		add("Descripción del PR: ", w.prBody)
		if w.jiraSummary != "" {
			b.WriteString("\nTicket " + w.ticket + ": " + w.jiraSummary)
		}
		add("Descripción del ticket: ", w.jiraDesc)
		if commits != "" {
			b.WriteString("\nCommits:\n" + commits)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "Write a very short descriptor, in %s, of what this development work is about. It is shown in a list of worktrees to recognize it at a glance.\n"+
		"Rules: 3 to 5 words and at most %d characters. No ticket or PR number, no prefixes like feat or fix, no quotes or final period. Start with a capital letter. Reply with the descriptor only.\n\n"+
		"Branch: %s", lang, limit, w.branch)
	add := func(label, v string) {
		if v != "" {
			b.WriteString("\n" + label + v)
		}
	}
	add("PR title: ", w.prTitle)
	add("PR description: ", w.prBody)
	if w.jiraSummary != "" {
		b.WriteString("\nTicket " + w.ticket + ": " + w.jiraSummary)
	}
	add("Ticket description: ", w.jiraDesc)
	if commits != "" {
		b.WriteString("\nCommits:\n" + commits)
	}
	return b.String()
}

func aiCache(key string) string { return filepath.Join(stateDir, "ai", key) }

// aiCooling: generation failed for the key less than the cooldown ago.
func aiCooling(key string) bool {
	age, ok := fileAge(aiCache(key) + ".fail")
	return ok && age < aiFailCooldown
}

func aiCached(key string) bool {
	_, err := os.Stat(aiCache(key))
	return err == nil
}

var (
	// perl's Unicode \s and \S: Go's \s is ASCII only.
	reNonSpace  = regexp.MustCompile(`[^\s\v\p{Z}\x{85}]`)
	reEdgeSpace = regexp.MustCompile(`^[\s\v\p{Z}\x{85}]+|[\s\v\p{Z}\x{85}]+$`)
	reLeadJunk  = regexp.MustCompile("^[\\s\\v\\p{Z}\\x{85}\"“”«»`*_'-]+")
	reTrailJunk = regexp.MustCompile("[\\s\\v\\p{Z}\\x{85}\"“”«»`*_'.]+$")
	reSpace     = regexp.MustCompile(`[\s\v\p{Z}\x{85}]`)
	reLastWord  = regexp.MustCompile(`[\s\v\p{Z}\x{85}]+[^\s\v\p{Z}\x{85}]+$`)
)

// cleanDescriptor is the first non-empty line of the model's answer, without
// quotes, markdown or a final period, cut at a word boundary to max
// characters.
func cleanDescriptor(answer string, max int) string {
	line := ""
	for _, l := range strings.SplitAfter(answer, "\n") {
		if reNonSpace.MatchString(l) {
			line = l
			break
		}
	}
	line = reEdgeSpace.ReplaceAllString(line, "")
	line = reLeadJunk.ReplaceAllString(line, "")
	line = reTrailJunk.ReplaceAllString(line, "")
	for utf8.RuneCountInString(line) > max && reSpace.MatchString(line) {
		line = reLastWord.ReplaceAllString(line, "")
	}
	return cutRunes(line, max)
}

// generate runs the model for the plan and caches the descriptor; on failure
// it leaves a .fail marker so later events wait out the cooldown.
func generate(w *workspace, p aiPlan) bool {
	cache := aiCache(p.key)
	ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBin, "-p", "--safe-mode", "--model", aiModel, "--tools", "",
		"--no-session-persistence", "--settings", `{"disableAllHooks":true}`)
	cmd.Env = append(os.Environ(), "MOSHI_SOCKET_PATH=/dev/null/moshi.sock")
	cmd.Stdin = strings.NewReader(p.prompt + "\n")
	out, _ := cmd.Output()
	desc := cleanDescriptor(string(out), p.limit)
	if desc == "" {
		logf("%s ai descriptor failed for %s", w.id, orDefault(w.branch, "?"))
		writeFileAtomic(cache+".fail", nil)
		return false
	}
	writeText(cache, desc)
	os.Remove(cache + ".fail")
	logf("%s ai descriptor: %s", w.id, desc)
	return true
}

// ---- the queue: one generator at a time ----

// aiLock is held by the process draining the queue. The kernel releases it
// when the process ends, however it ends, so there is no stale lock to take
// over.
var aiLock *os.File

func takeLock() bool {
	f, err := os.OpenFile(filepath.Join(stateDir, "ai.flock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return false // a live generator will drain the queue
	}
	aiLock = f
	return true
}

func releaseLock() {
	if aiLock != nil {
		syscall.Flock(int(aiLock.Fd()), syscall.LOCK_UN)
		aiLock.Close()
		aiLock = nil
	}
}

func enqueue(id string) {
	writeFileAtomic(filepath.Join(stateDir, "queue", id), nil)
}

func nextQueued() (string, bool) {
	entries, err := os.ReadDir(filepath.Join(stateDir, "queue"))
	if err != nil {
		return "", false
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

// drainQueue generates every queued descriptor. Each workspace is collected
// again right before its generation, so a branch switched meanwhile never
// gets an old answer, and two workspaces with the same key share one call.
func drainQueue() {
	for {
		if _, ok := nextQueued(); !ok {
			return
		}
		if !takeLock() {
			return
		}
		for {
			id, ok := nextQueued()
			if !ok {
				break
			}
			os.Remove(filepath.Join(stateDir, "queue", id))
			list, err := workspaceList()
			if err != nil {
				continue
			}
			w := collect(list, id)
			p := planAI(w)
			if p.key != "" && !aiCached(p.key) && !aiCooling(p.key) {
				// The branch may have changed while the model answered: start
				// over with what the checkout is now instead of publishing the
				// old one.
				if generate(w, p) && gitOut(w.path, "branch", "--show-current") != w.branch {
					enqueue(id)
					continue
				}
			}
			// Published even without a new descriptor: the workspace was
			// collected afresh, and what was published before may be from
			// another branch.
			publish(w, p)
		}
		releaseLock()
		// Something may have been queued between the last check and the release.
	}
}
