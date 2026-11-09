//go:build unix

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestConfigAccountsOmittedWhenEmpty(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "cfg.json")
	cfg, _, err := loadConfig(path) // default config written by clauder
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	cfg, _, _ = loadConfig(path)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("config changed on save:\n%s\n---\n%s", before, after)
	}
	for _, k := range []string{"accounts", "default_account", "folder_accounts"} {
		if bytes.Contains(after, []byte(k)) {
			t.Errorf("%s written for an empty config", k)
		}
	}
}

func TestConfigAccountsRoundTrip(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "cfg.json")
	writeFile(t, path, `{"roots":[],"zz":1,"accounts":[{"name":"personal","config_dir":"~/.claude"},{"name":"work","config_dir":"~/.claude-work"}],
		"default_account":"personal","folder_accounts":{"~/work":"work","/opt/x":"personal"}}`)
	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts[1].ConfigDir != filepath.Join(home, ".claude-work") || cfg.FolderAccounts[filepath.Join(home, "work")] != "work" {
		t.Fatalf("not expanded: %+v %v", cfg.Accounts, cfg.FolderAccounts)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	order := []string{`"git"`, `"accounts"`, `"default_account"`, `"folder_accounts"`, `"zz"`}
	for i := 1; i < len(order); i++ {
		if strings.Index(s, order[i-1]) > strings.Index(s, order[i]) {
			t.Fatalf("key order wrong:\n%s", s)
		}
	}
	m := readJSON(t, path)
	if got := m["folder_accounts"]; !reflect.DeepEqual(got, map[string]any{"~/work": "work", "/opt/x": "personal"}) {
		t.Fatalf("folder_accounts saved as %v", got)
	}
	accts := m["accounts"].([]any)
	if accts[0].(map[string]any)["config_dir"] != "~/.claude" || accts[1].(map[string]any)["config_dir"] != "~/.claude-work" {
		t.Fatalf("accounts saved as %v", accts)
	}
	if m["default_account"] != "personal" {
		t.Fatalf("default_account = %v", m["default_account"])
	}
}

func TestResolveAccount(t *testing.T) {
	setHome(t)
	cfg := &Config{file: filepath.Join(t.TempDir(), "c.json"),
		Accounts:       []Account{{Name: "personal"}, {Name: "Work", ConfigDir: "/w"}},
		DefaultAccount: "personal",
		FolderAccounts: map[string]string{"/p": "work", "/p/oss": "personal", "/p/x": "gone", "/q/": "work"}}
	cases := []struct{ path, acc, from string }{
		{"/p", "Work", "/p"},                 // exact
		{"/p/api/sub", "Work", "/p"},         // inherited
		{"/p/oss/lib", "personal", "/p/oss"}, // deeper overrides
		{"/p/x/y", "Work", "/p"},             // dangling skipped -> parent
		{"/px", "personal", ""},              // not a prefix match
		{"/q/a", "Work", "/q/"},              // key with trailing slash
		{"/elsewhere", "personal", ""},       // global default
	}
	for _, c := range cases {
		acc, from := cfg.ResolveAccount(c.path)
		if acc == nil || acc.Name != c.acc || from != c.from {
			t.Errorf("%s: got %v %q, want %s %q", c.path, acc, from, c.acc, c.from)
		}
	}
	cfg.DefaultAccount = "nope" // dangling default
	if acc, _ := cfg.ResolveAccount("/elsewhere"); acc != nil {
		t.Errorf("dangling default resolved to %v", acc)
	}
	if acc, _ := (&Config{FolderAccounts: map[string]string{"/p": "x"}}).ResolveAccount("/p"); acc != nil {
		t.Error("no accounts must resolve to nil")
	}
}

func TestAccountMutations(t *testing.T) {
	home := setHome(t)
	cfg, _, err := loadConfig(filepath.Join(home, "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddAccount("a", "/a")
	cfg.AddAccount("b", "/b")
	if cfg.DefaultAccount != "a" {
		t.Fatalf("first account should be default, got %q", cfg.DefaultAccount)
	}
	if validAccountName(cfg, "B", -1) == nil || validAccountName(cfg, "x/y", -1) == nil || validAccountName(cfg, " ", -1) == nil {
		t.Error("validation too lax")
	}
	if validAccountName(cfg, "b", 1) != nil {
		t.Error("renaming to own name must be allowed")
	}
	cfg.SetFolderAccount("/f", "a")
	cfg.SetFolderAccount("/g", "b")
	cfg.RenameAccount(0, "alpha")
	if cfg.DefaultAccount != "alpha" || cfg.FolderAccounts["/f"] != "alpha" {
		t.Fatalf("rename not propagated: %q %v", cfg.DefaultAccount, cfg.FolderAccounts)
	}
	cfg.DeleteAccount(0)
	if cfg.DefaultAccount != "" || len(cfg.FolderAccounts) != 1 || cfg.FolderAccounts["/g"] != "b" {
		t.Fatalf("delete not propagated: %q %v", cfg.DefaultAccount, cfg.FolderAccounts)
	}
	cfg.ClearFolderAccount("/g")
	c := reload(t, cfg)
	if len(c.Accounts) != 1 || c.Accounts[0].Name != "b" || len(c.FolderAccounts) != 0 || c.DefaultAccount != "" {
		t.Fatalf("saved: %+v", c)
	}
}

func TestTierLabel(t *testing.T) {
	cases := []struct {
		o    oauthAccount
		want string
	}{
		{oauthAccount{OrganizationType: "claude_team", UserRateLimitTier: "default_claude_max_5x"}, "Team 5x"},
		{oauthAccount{OrganizationType: "claude_max", UserRateLimitTier: "default_claude_max_20x"}, "Max 20x"},
		{oauthAccount{OrganizationType: "claude_pro", UserRateLimitTier: "default_claude_ai"}, "Pro"},
		{oauthAccount{OrganizationType: "claude_enterprise", OrganizationRateLimitTier: "org_5x"}, "Enterprise 5x"},
		{oauthAccount{OrganizationType: "claude_max", UserRateLimitTier: "tier_x", OrganizationRateLimitTier: "a_20x"}, "Max 20x"},
		{oauthAccount{OrganizationType: "claude_free"}, "claude_free"},
		{oauthAccount{OrganizationType: "claude_max", UserRateLimitTier: "max_05x"}, "Max"},
		{oauthAccount{}, ""},
	}
	for _, c := range cases {
		if got := tierLabel(c.o); got != c.want {
			t.Errorf("%+v: %q, want %q", c.o, got, c.want)
		}
	}
}

func TestAccountInfo(t *testing.T) {
	home := setHome(t)
	writeFile(t, filepath.Join(home, ".claude.json"), `{"primaryApiKey":"secret","oauthAccount":{"emailAddress":"me@x.cz",
		"organizationName":"Org","organizationType":"claude_team","userRateLimitTier":"default_claude_max_5x"}}`)
	writeFile(t, filepath.Join(home, "legacy", ".config.json"), `{"oauthAccount":{"emailAddress":"old@x.cz","organizationType":"claude_pro"}}`)
	mkdirs(t, home, "fresh")
	cases := map[string]string{
		"":                               "me@x.cz · Team 5x",
		filepath.Join(home, ".claude"):   "me@x.cz · Team 5x",
		filepath.Join(home, "legacy"):    "old@x.cz · Pro",
		filepath.Join(home, "fresh"):     "not logged in",
		filepath.Join(home, "no", "dir"): "not logged in",
	}
	for dir, want := range cases {
		a := &Account{Name: "n", ConfigDir: dir}
		if got := infoFor(a).summary(); got != want {
			t.Errorf("%q: %q, want %q", dir, got, want)
		}
	}
	if got := accountDesc(&Account{Name: "p"}); got != "p (me@x.cz · Team 5x)" || strings.Contains(got, "secret") {
		t.Errorf("desc = %q", got)
	}
}

func TestBuildEnv(t *testing.T) {
	home := setHome(t)
	base := []string{"A=1", "PWD=/old", "CLAUDE_CONFIG_DIR=/inherited"}
	get := func(env []string, k string) (string, bool) {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, k+"="); ok {
				return v, true
			}
		}
		return "", false
	}
	env := buildEnv(base, "/d", nil) // no account: untouched
	if v, _ := get(env, "CLAUDE_CONFIG_DIR"); v != "/inherited" {
		t.Errorf("untouched: %v", env)
	}
	if v, _ := get(env, "PWD"); v != "/d" || slices.Contains(env, "PWD=/old") {
		t.Errorf("PWD: %v", env)
	}
	for _, dir := range []string{"", filepath.Join(home, ".claude")} {
		env = buildEnv(base, "/d", &Account{ConfigDir: dir}) // default config: unset
		if _, ok := get(env, "CLAUDE_CONFIG_DIR"); ok {
			t.Errorf("%q: CLAUDE_CONFIG_DIR not removed: %v", dir, env)
		}
	}
	env = buildEnv(base, "/d", &Account{ConfigDir: filepath.Join(home, ".claude-work")})
	if v, _ := get(env, "CLAUDE_CONFIG_DIR"); v != filepath.Join(home, ".claude-work") || len(env) != 3 {
		t.Errorf("set: %v", env)
	}
	if !slices.Equal(base, []string{"A=1", "PWD=/old", "CLAUDE_CONFIG_DIR=/inherited"}) {
		t.Error("input modified")
	}
}

func TestLinkShared(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "a", "dst")
	mkdirs(t, src, "agents", "skills", "projects")
	writeFile(t, filepath.Join(src, "CLAUDE.md"), "x")
	writeFile(t, filepath.Join(src, "settings.json"), "{}")
	writeFile(t, filepath.Join(src, ".credentials.json"), "secret")
	n, err := linkShared(src, filepath.Join(d, "a", "dst"))
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if st, _ := os.Stat(dst); st.Mode().Perm() != 0o700 {
		t.Errorf("dst mode %v", st.Mode().Perm())
	}
	if l, _ := os.Readlink(filepath.Join(dst, "agents")); l != filepath.Join(src, "agents") {
		t.Errorf("agents -> %q", l)
	}
	for _, name := range []string{"projects", ".credentials.json", "hooks"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s must not be linked", name)
		}
	}
	// existing targets are kept; a second run links only what is new
	os.Remove(filepath.Join(dst, "CLAUDE.md"))
	writeFile(t, filepath.Join(dst, "CLAUDE.md"), "own")
	mkdirs(t, src, "rules")
	if n, err = linkShared(src, dst); err != nil || n != 1 {
		t.Fatalf("second run n=%d err=%v", n, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "CLAUDE.md")); string(data) != "own" {
		t.Error("existing CLAUDE.md replaced")
	}
}

func TestUnknownAccountExits2(t *testing.T) {
	cfg, _ := fixture(t)
	if code := run([]string{"--config", cfg.file, "--account", "nope", "--find", "beta"}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// accountsFixture adds accounts "personal" (default login, global default)
// and "work" to the fixture.
func accountsFixture(t *testing.T) (*Config, string) {
	t.Helper()
	cfg, root := fixture(t)
	home := homeDir()
	writeFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"me@x.cz","organizationType":"claude_max","userRateLimitTier":"default_claude_max_20x"}}`)
	cfg.Accounts = []Account{{Name: "personal", ConfigDir: filepath.Join(home, ".claude")}, {Name: "work", ConfigDir: filepath.Join(home, ".claude-work")}}
	cfg.DefaultAccount = "personal"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfg, root
}

func TestTUIAccountSelector(t *testing.T) {
	cfg, root := accountsFixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Send(key(tea.KeyRight)) // into work; alpha highlighted
	waitFor(t, tm, "account: personal (me@x.cz · Max 20x) · default")
	tm.Type("u")
	waitFor(t, tm, "account for work/alpha")
	tm.Type("j") // work
	tm.Type("s") // set as alpha's default, back to browse
	waitFor(t, tm, "alpha › [work]")
	tm.Send(key(tea.KeyRight)) // into alpha: one inherits
	waitFor(t, tm, "account: work (not logged in) · from work/alpha")
	tm.Type("u")
	waitFor(t, tm, "from work/alpha")
	tm.Type("k") // personal, run once
	tm.Send(key(tea.KeyEnter))
	m := finalModel(t, tm)
	if m.chosen != filepath.Join(root, "alpha", "one") || m.chosenAcct != "personal" {
		t.Fatalf("chosen=%q acct=%q", m.chosen, m.chosenAcct)
	}
	if got := reload(t, cfg).FolderAccounts; !reflect.DeepEqual(got, map[string]string{filepath.Join(root, "alpha"): "work"}) {
		t.Fatalf("folder_accounts = %v", got)
	}

	// c clears the assignment; search opens the selector with ctrl+o
	tm = start(t, reload(t, cfg), startOpts{})
	tm.Type("/alpha")
	waitFor(t, tm, "❯ work/alpha [work]")
	tm.Send(key(tea.KeyCtrlO))
	waitFor(t, tm, "set here")
	tm.Type("c")
	waitFor(t, tm, "work/alpha inherits its account again")
	tm.Send(key(tea.KeyEsc))
	tm.Type("q")
	finalModel(t, tm)
	if got := reload(t, cfg).FolderAccounts; len(got) != 0 {
		t.Fatalf("folder_accounts = %v", got)
	}
}

func TestTUINoAccountsNoNoise(t *testing.T) {
	cfg, _ := fixture(t)
	m := newModel(cfg, nil, startOpts{})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	if v := m.View(); strings.Contains(v, "account") {
		t.Fatalf("account UI without accounts:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	if m.mode != modeBrowse || !strings.Contains(m.msg, "no accounts yet") {
		t.Fatalf("mode=%d msg=%q", m.mode, m.msg)
	}
}

func TestTUISettingsAccounts(t *testing.T) {
	cfg, _ := fixture(t)
	home := homeDir()
	mkdirs(t, home, ".claude/skills")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), "{}")
	tm := start(t, cfg, startOpts{mode: modeSettings})
	tm.Send(key(tea.KeyTab))
	waitFor(t, tm, "roots [accounts]")
	tm.Type("a")
	tm.Type("personal")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "config dir: ~/.claude")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "added account personal · uses the default Claude login")
	tm.Type("a")
	tm.Type("Personal") // duplicate
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "already exists")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("work")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "config dir: ~/.claude-work")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "2 shared items linked into ~/.claude-work")
	tm.Send(key(tea.KeyEnter)) // work becomes the default
	waitFor(t, tm, "default account work")
	tm.Type("k") // personal
	tm.Type("d")
	waitFor(t, tm, "delete account personal? ~/.claude is kept. y/n")
	tm.Type("y")
	waitFor(t, tm, "deleted account personal")
	tm.Send(key(tea.KeyTab))
	waitFor(t, tm, "[roots] accounts")
	tm.Type("q")
	waitFor(t, tm, "clauder")
	tm.Type("q")
	finalModel(t, tm)
	c := reload(t, cfg)
	want := []Account{{Name: "work", ConfigDir: filepath.Join(home, ".claude-work")}}
	if !reflect.DeepEqual(c.Accounts, want) || c.DefaultAccount != "work" {
		t.Fatalf("accounts=%+v default=%q", c.Accounts, c.DefaultAccount)
	}
	if l, err := os.Readlink(filepath.Join(home, ".claude-work", "skills")); err != nil || l != filepath.Join(home, ".claude", "skills") {
		t.Fatalf("skills link: %q %v", l, err)
	}
	if !isDir(filepath.Join(home, ".claude")) {
		t.Fatal("deleting an account must keep its dir")
	}
}

func TestCheckConfigDir(t *testing.T) {
	home := setHome(t)
	mkdirs(t, home, ".claude/sub", "empty", "junk/x", "old/projects", "linked")
	writeFile(t, filepath.Join(home, "linked", "settings.json"), "{}")
	if err := os.Symlink(home, filepath.Join(home, "homealias")); err != nil {
		t.Fatal(err)
	}
	for dir, ok := range map[string]bool{
		"~/.claude": true, "~/.claude-work": true, "~/empty": true, "~/old": true, "~/linked": true,
		"~": false, "~/homealias": false, "/": false, filepath.Dir(home): false,
		"~/.claude/sub": false, "~/.claude/new": false, "~/junk": false,
	} {
		if err := checkConfigDir(expandPath(dir)); (err == nil) != ok {
			t.Errorf("%s: err=%v", dir, err)
		}
	}
}

func TestTUISettingsAccountDirChecks(t *testing.T) {
	cfg, _ := fixture(t)
	home := homeDir()
	mkdirs(t, home, ".claude/skills", ".claude-work")
	if err := os.Symlink(filepath.Join(home, ".claude-work"), filepath.Join(home, "alias")); err != nil {
		t.Fatal(err)
	}
	cfg.Accounts = []Account{{Name: "work", ConfigDir: filepath.Join(home, ".claude-work")}}
	cfg.Save()
	tm := start(t, cfg, startOpts{mode: modeSettings})
	tm.Send(key(tea.KeyTab))
	tm.Type("a")
	tm.Type("  home  ") // trimmed
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "config dir: ~/.claude-home ")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("~")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "~ is your home dir")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("~/alias") // symlink to work's dir
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "~/alias is already used by account work")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("~/.claude-home")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "added account home")
	tm.Type("q")
	tm.Type("q")
	finalModel(t, tm)
	c := reload(t, cfg)
	if len(c.Accounts) != 2 || c.Accounts[1].Name != "home" {
		t.Fatalf("accounts = %+v", c.Accounts)
	}
	for _, name := range []string{"skills", "CLAUDE.md"} {
		if _, err := os.Lstat(filepath.Join(home, name)); err == nil {
			t.Errorf("~/%s must not be created", name)
		}
	}
}

func TestTUIAccountFlagOverride(t *testing.T) {
	cfg, _ := accountsFixture(t)
	m := newModel(cfg, nil, startOpts{account: "work"})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	if v := m.View(); !strings.Contains(v, "account: work (not logged in) · --account") {
		t.Fatalf("override not shown:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	if m.mode != modeAccount || m.acct.sel != 1 {
		t.Fatalf("mode=%d sel=%d", m.mode, m.acct.sel)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp}) // selector choice wins over the flag
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.chosenAcct != "personal" {
		t.Fatalf("chosenAcct = %q", m.chosenAcct)
	}
}
