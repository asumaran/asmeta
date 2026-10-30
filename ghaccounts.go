package main

// The org→account map (~/.config/gh-accounts, versioned in dotfiles-bash):
// which gh account sees each GitHub org, so the PR refresh queries every org
// with an account that has access instead of whatever account is active.
// One line `<org> <account>`; `default` answers for unlisted orgs. Shared
// contract with asreviewer; the spec lives with the dotfiles module.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ghTokenTimeout caps one `gh auth token` call; it runs with the PR cache
// lock taken, so it must never hang on a stuck keychain.
const ghTokenTimeout = 10 * time.Second

// ghAccountsPath is $GH_ACCOUNTS_MAP (tests, overrides), else the installed
// map. An empty or blank variable counts as unset.
func ghAccountsPath() string {
	if p := strings.TrimSpace(os.Getenv("GH_ACCOUNTS_MAP")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh-accounts")
}

// loadGHAccounts reads the map. A missing file degrades silently to the
// pre-map behavior (nil map, no default: one query with the active account);
// a file that exists but cannot be read is logged, once per refresh.
func loadGHAccounts() (orgs map[string]string, def string) {
	path := ghAccountsPath()
	if path == "" {
		return nil, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("gh-accounts map unreadable: %v", err)
		}
		return nil, ""
	}
	return parseGHAccounts(string(data))
}

// parseGHAccounts parses the two-column format: blank lines and #-comments
// ignored, CRLF tolerated, orgs lowercased, first match wins, a line with
// other than two fields is ignored with a warning, never an error.
func parseGHAccounts(data string) (orgs map[string]string, def string) {
	orgs = map[string]string{}
	defSet := false
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			logf("gh-accounts map: ignoring malformed line %q", line)
			continue
		}
		org, account := strings.ToLower(fields[0]), fields[1]
		if org == "default" {
			if !defSet {
				def, defSet = account, true
			}
			continue
		}
		if _, ok := orgs[org]; !ok {
			orgs[org] = account
		}
	}
	return orgs, def
}

// accountFor is the account that sees owner's repos: its org's row, else the
// default, else "" (no identity: the active account is used).
func accountFor(orgs map[string]string, def, owner string) string {
	if a, ok := orgs[strings.ToLower(owner)]; ok {
		return a
	}
	return def
}

// ghAuthToken resolves one account's token, resolved anew each refresh so a
// rotated token self-heals. A package var so tests inject it (as ghGraphQL).
var ghAuthToken = func(ctx context.Context, account string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "-u", account)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh auth token -u %s: %s", account,
			orDefault(firstLineOf(errb.String()), err.Error()))
	}
	token := strings.TrimSpace(out.String())
	if token == "" {
		return "", fmt.Errorf("gh auth token -u %s: empty token", account)
	}
	return token, nil
}
