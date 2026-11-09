//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

// sharedClaudeItems are linked from ~/.claude into a new account's config dir.
// Login, .claude.json, history, projects and sessions stay per account.
var sharedClaudeItems = []string{"CLAUDE.md", "settings.json", "keybindings.json",
	"agents", "commands", "hooks", "output-styles", "plugins", "rules", "skills"}

// oauthAccount is the subset of Claude's global config clauder reads. The
// file holds secrets too; only these fields are ever decoded.
type oauthAccount struct {
	EmailAddress              string `json:"emailAddress"`
	OrganizationName          string `json:"organizationName"`
	OrganizationType          string `json:"organizationType"`
	SeatTier                  string `json:"seatTier"`
	UserRateLimitTier         string `json:"userRateLimitTier"`
	OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
	BillingType               string `json:"billingType"`
}

type accountInfo struct {
	loggedIn bool
	email    string
	tier     string
}

var (
	infoMu    sync.Mutex
	infoCache = map[string]accountInfo{} // by global config file path
)

func defaultClaudeDir() string { return filepath.Join(homeDir(), ".claude") }

// isDefaultClaudeDir reports whether dir means Claude's default config, which
// must run with CLAUDE_CONFIG_DIR unset to reuse the default login.
func isDefaultClaudeDir(dir string) bool {
	return dir == "" || filepath.Clean(expandPath(dir)) == defaultClaudeDir()
}

// dirLabel is the account's config dir for display.
func (a *Account) dirLabel() string {
	if a.ConfigDir == "" {
		return "~/.claude"
	}
	return safePath(a.ConfigDir)
}

// claudeJSON is the global config file holding an account's oauthAccount.
func claudeJSON(dir string) string {
	if isDefaultClaudeDir(dir) {
		return filepath.Join(homeDir(), ".claude.json")
	}
	dir = expandPath(dir)
	p := filepath.Join(dir, ".claude.json")
	if !fileExists(p) {
		if legacy := filepath.Join(dir, ".config.json"); fileExists(legacy) {
			return legacy
		}
	}
	return p
}

// infoFor reads the account's login details once per process.
func infoFor(a *Account) accountInfo {
	path := claudeJSON(a.ConfigDir)
	infoMu.Lock()
	defer infoMu.Unlock()
	if i, ok := infoCache[path]; ok {
		return i
	}
	i := readAccountInfo(path)
	infoCache[path] = i
	return i
}

func readAccountInfo(path string) accountInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return accountInfo{}
	}
	var g struct {
		OAuthAccount *oauthAccount `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &g) != nil || g.OAuthAccount == nil {
		return accountInfo{}
	}
	return accountInfo{loggedIn: true, email: g.OAuthAccount.EmailAddress, tier: tierLabel(*g.OAuthAccount)}
}

// tierLabel is "Max 20x", "Team 5x", "Pro", …: the organization type plus a
// rate-limit multiplier when the tier names one.
func tierLabel(o oauthAccount) string {
	base := o.OrganizationType
	switch t := strings.TrimPrefix(base, "claude_"); t {
	case "max", "pro", "team", "enterprise":
		base = strings.ToUpper(t[:1]) + t[1:]
	}
	for _, t := range []string{o.UserRateLimitTier, o.OrganizationRateLimitTier} {
		if m := multiplier(t); m != "" {
			return strings.TrimSpace(base + " " + m)
		}
	}
	return base
}

// multiplier returns "5x" for a tier ending in "_5x", else "".
func multiplier(tier string) string {
	i := strings.LastIndex(tier, "_")
	n, ok := strings.CutSuffix(tier[i+1:], "x")
	if i < 0 || !ok || n == "" {
		return ""
	}
	if v, err := strconv.Atoi(n); err != nil || v < 1 || strconv.Itoa(v) != n {
		return ""
	}
	return n + "x"
}

// summary is "email · tier", or "not logged in".
func (i accountInfo) summary() string {
	if !i.loggedIn {
		return "not logged in"
	}
	parts := slices.DeleteFunc([]string{i.email, i.tier}, func(s string) bool { return s == "" })
	if len(parts) == 0 {
		return "logged in"
	}
	return strings.Join(parts, " · ")
}

// accountDesc is "name (email · tier)", unsanitized.
func accountDesc(a *Account) string {
	return a.Name + " (" + infoFor(a).summary() + ")"
}

// buildEnv returns environ with PWD=dir and CLAUDE_CONFIG_DIR set for acc:
// removed for Claude's default config, untouched when acc is nil.
func buildEnv(environ []string, dir string, acc *Account) []string {
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		return strings.HasPrefix(kv, "PWD=") || acc != nil && strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=")
	})
	env = append(env, "PWD="+dir)
	if acc != nil && !isDefaultClaudeDir(acc.ConfigDir) {
		d := expandPath(acc.ConfigDir)
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		env = append(env, "CLAUDE_CONFIG_DIR="+d)
	}
	return env
}

// realPath is p made absolute with symlinks resolved as far as it exists.
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	rest := ""
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(q) == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
	}
}

// within reports whether p is strictly inside dir.
func within(p, dir string) bool { return strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/") }

// checkConfigDir rejects a new account's config dir that would clash with
// the default Claude config ($HOME, an ancestor of ~/.claude, a dir inside
// it) or that holds unrelated files. ~/.claude itself is fine.
func checkConfigDir(p string) error {
	rp, def := realPath(p), realPath(defaultClaudeDir())
	switch {
	case rp == def:
		return nil
	case rp == realPath(homeDir()):
		return errors.New(tildePath(p) + " is your home dir")
	case within(def, rp):
		return errors.New(tildePath(p) + " contains ~/.claude")
	case within(rp, def):
		return errors.New(tildePath(p) + " is inside ~/.claude")
	}
	if entries, err := os.ReadDir(p); err == nil && len(entries) > 0 && !looksLikeClaudeDir(p) {
		return errors.New(tildePath(p) + " is not empty and doesn't look like a Claude config dir")
	}
	return nil
}

func looksLikeClaudeDir(p string) bool {
	for _, name := range []string{".claude.json", "settings.json", "projects"} {
		if _, err := os.Lstat(filepath.Join(p, name)); err == nil {
			return true
		}
	}
	return false
}

// linkShared creates dst (0700) if missing and symlinks the shared items of
// src into it. Items missing in src or already present in dst are skipped,
// nothing is replaced. It returns how many links it made.
func linkShared(src, dst string) (int, error) {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return 0, err
	}
	n := 0
	for _, name := range sharedClaudeItems {
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		if _, err := os.Lstat(from); err != nil {
			continue
		}
		if _, err := os.Lstat(to); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Symlink(from, to); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// truncWidth cuts s to at most w cells, ending in "…" when cut.
func truncWidth(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
