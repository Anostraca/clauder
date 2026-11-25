//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// rec is one JSONL record.
func rec(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeSession writes a session file into configDir's project dir for
// startup folder project and sets its mtime.
func writeSession(t *testing.T, configDir, project, id string, when time.Time, lines ...string) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", projectKey(project))
	mkdirs(t, dir, ".")
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func userRec(t *testing.T, cwd, text string) string {
	return rec(t, map[string]any{"type": "user", "cwd": cwd, "message": map[string]any{"role": "user", "content": text}})
}

func sessionIDs(list []Session) []string {
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestProjectKey(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/x/Drive-Sync/a": "-Users-x-Drive-Sync-a",
		"/p/a b.c_d~e":          "-p-a-b-c-d-e",
		"/p/\u00e9":             "-p--",  // one UTF-16 unit
		"/p/\U0001F600":         "-p---", // a surrogate pair
		"/p/a-b":                "-p-a-b",
		"/p/a/b":                "-p-a-b",
		"/Work/API2/v1":         "-Work-API2-v1",
		"/p/x\u00a0y\u3000":     "-p-x-y-",
	} {
		if got := projectKey(in); got != want {
			t.Errorf("projectKey(%q) = %q, want %q", in, got, want)
		}
	}

	if !keyMatches("-p-api", "-p-api") || !keyMatches("-p-api-sub", "-p-api") || keyMatches("-p-apix", "-p-api") {
		t.Error("short key prefix rule")
	}
	long := "/p/" + strings.Repeat("a", 250)
	key := projectKey(long)
	for _, name := range []string{key[:200] + "-1a2b3c", projectKey(long + "/sub")[:200] + "-9f9f"} {
		if !keyMatches(name, key) {
			t.Errorf("long key should match %q", name)
		}
	}
	if keyMatches("-p-"+strings.Repeat("b", 197)+"-1a2b", key) {
		t.Error("long key matched another folder")
	}
	edge := "/" + strings.Repeat("e", 199) // key exactly 200 chars: a subfolder is hashed right after it
	if k := projectKey(edge); len(k) != 200 || !keyMatches(k+"-hash", k) {
		t.Errorf("200-char key: len %d", len(k))
	}
}

func TestFindSessionsScope(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	p := filepath.Join(home, "p")
	mkdirs(t, p, "api/sub", "api-v2", "a/b", "a-b")
	claude := filepath.Join(home, ".claude")
	now := time.Now()
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	api, sub, apiV2 := filepath.Join(p, "api"), filepath.Join(p, "api/sub"), filepath.Join(p, "api-v2")
	ab, aDashB := filepath.Join(p, "a/b"), filepath.Join(p, "a-b")
	writeSession(t, claude, api, "s-api", at(1), userRec(t, api, "api work"))
	writeSession(t, claude, sub, "s-sub", at(2), userRec(t, sub, "sub work"))
	writeSession(t, claude, apiV2, "s-v2", at(3), userRec(t, apiV2, "sibling"))
	writeSession(t, claude, aDashB, "s-adashb", at(4), userRec(t, aDashB, "same key"))
	writeSession(t, claude, ab, "s-ab", at(5), userRec(t, ab, "a/b"))
	writeSession(t, claude, api, "s-nocwd", at(0), rec(t, map[string]any{"type": "custom-title", "customTitle": "x"}))
	side := filepath.Join(claude, "projects", projectKey(api), "s-api", "subagents") // sidechains are not sessions
	mkdirs(t, side, ".")
	os.WriteFile(filepath.Join(side, "agent-1.jsonl"), []byte(userRec(t, api, "side")+"\n"), 0o644)
	os.WriteFile(filepath.Join(claude, "projects", projectKey(api), "notes.txt"), []byte("x"), 0o644)

	list, cut := findSessions(&Config{}, []string{api})
	if got := sessionIDs(list); cut || !reflect.DeepEqual(got, []string{"s-api", "s-sub"}) {
		t.Fatalf("api scope: %v cut=%v", got, cut)
	}
	if list[1].Dir != sub || list[1].Title != "sub work" || list[1].Account != "" || !list[1].When.Equal(at(2)) {
		t.Errorf("session = %+v", list[1])
	}
	list, _ = findSessions(&Config{}, []string{ab})
	if got := sessionIDs(list); !reflect.DeepEqual(got, []string{"s-ab"}) {
		t.Errorf("a/b scope: %v", got)
	}
	list, _ = findSessions(&Config{}, []string{ab, apiV2})
	if got := sessionIDs(list); !reflect.DeepEqual(got, []string{"s-v2", "s-ab"}) {
		t.Errorf("two bases: %v", got)
	}

	// $CLAUDE_CONFIG_DIR replaces ~/.claude when no accounts are configured
	other := filepath.Join(home, "other")
	writeSession(t, other, api, "s-other", at(0), userRec(t, api, "other dir"))
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	list, _ = findSessions(&Config{}, []string{api})
	if got := sessionIDs(list); !reflect.DeepEqual(got, []string{"s-other"}) {
		t.Errorf("CLAUDE_CONFIG_DIR: %v", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "missing"))
	if list, cut := findSessions(&Config{}, []string{api}); len(list) != 0 || cut {
		t.Errorf("missing config dir: %v", list)
	}
}

func TestFindSessionsSymlinkedBase(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	mkdirs(t, home, "real/proj/x")
	if err := os.Symlink(filepath.Join(home, "real"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, "link/proj")
	phys := realPath(filepath.Join(home, "real/proj")) // what claude records
	claude := filepath.Join(home, ".claude")
	now := time.Now()
	writeSession(t, claude, phys+"/x", "s-x", now, userRec(t, phys+"/x", "inside"))
	writeSession(t, claude, phys, "s-base", now.Add(-time.Minute), userRec(t, phys, "base"))
	list, _ := findSessions(&Config{}, []string{base})
	if len(list) != 2 || list[0].Dir != filepath.Join(base, "x") || list[1].Dir != base {
		t.Fatalf("list = %+v", list)
	}
}

func TestReadSessionTitle(t *testing.T) {
	d := t.TempDir()
	custom := func(s string) string {
		return rec(t, map[string]any{"type": "custom-title", "customTitle": s, "sessionId": "x"})
	}
	ai := func(s string) string { return rec(t, map[string]any{"type": "ai-title", "aiTitle": s}) }
	prompt := func(s string) string { return rec(t, map[string]any{"type": "last-prompt", "lastPrompt": s}) }
	user := `{"type":"user","cwd":"/w","message":{"role":"user","content":"  first\n\tprompt  "}}`
	meta := `{"type":"user","cwd":"/w","isMeta":true,"message":{"content":"meta"}}`
	side := `{"type":"user","cwd":"/w","isSidechain":true,"message":{"content":"side"}}`
	blocks := `{"type":"user","cwd":"/w","message":{"content":[{"type":"text","text":"blocks"}]}}`
	cases := []struct {
		lines []string
		want  string
	}{
		{[]string{user, ai("AI"), custom("one"), prompt("p"), custom("two"), ai("AI 2")}, "two"},
		{[]string{user, ai("AI 1"), prompt("p"), ai("AI\n 2")}, "AI 2"},
		{[]string{user, prompt("last one"), `{"type":"last-prompt"}`, prompt("  ")}, "last one"},
		{[]string{meta, side, blocks, user}, "first prompt"},
		{[]string{meta, `not json`, `{"type":"user","cwd":"/w"`}, "(untitled)"},
		{[]string{custom("  "), user}, "first prompt"},
		{[]string{custom("esc \x1b[2J here")}, "esc \x1b[2J here"}, // sanitized when rendered
	}
	for i, c := range cases {
		p := filepath.Join(d, fmt.Sprintf("%d.jsonl", i))
		os.WriteFile(p, []byte(strings.Join(c.lines, "\n")), 0o644) // no trailing newline
		cwd, title := readSession(p)
		if title != c.want {
			t.Errorf("case %d: title = %q, want %q", i, title, c.want)
		}
		if i < 6 && cwd != "/w" {
			t.Errorf("case %d: cwd = %q", i, cwd)
		}
	}
	long := strings.Repeat("word ", 1000)
	os.WriteFile(filepath.Join(d, "long.jsonl"), []byte(custom(long)), 0o644)
	if _, title := readSession(filepath.Join(d, "long.jsonl")); len([]rune(title)) != maxTitleLen {
		t.Errorf("long title: %d runes", len([]rune(title)))
	}
}

func TestReadSessionHugeLines(t *testing.T) {
	d := t.TempDir()
	huge := `{"type":"assistant","cwd":"/later","message":{"content":"` + strings.Repeat("x", 3*sessionChunk) + `"}}`
	first := `{"type":"user","cwd":"/w","message":{"content":"hello"}}`
	p := filepath.Join(d, "a.jsonl")
	os.WriteFile(p, []byte(strings.Join([]string{first, huge, huge, `{"type":"ai-title","aiTitle":"tail title"}`}, "\n")+"\n"), 0o644)
	if cwd, title := readSession(p); cwd != "/w" || title != "tail title" {
		t.Errorf("head+tail: cwd=%q title=%q", cwd, title)
	}

	// the first line alone is larger than the head chunk: cwd comes from the tail
	p = filepath.Join(d, "b.jsonl")
	os.WriteFile(p, []byte(huge+"\n"+`{"type":"user","cwd":"/t","message":{"content":"tail prompt"}}`), 0o644)
	if cwd, title := readSession(p); cwd != "/t" || title != "tail prompt" {
		t.Errorf("tail only: cwd=%q title=%q", cwd, title)
	}

	// one single huge line: nothing usable, no failure
	p = filepath.Join(d, "c.jsonl")
	os.WriteFile(p, []byte(huge), 0o644)
	if cwd, title := readSession(p); cwd != "" || title != "(untitled)" {
		t.Errorf("one line: cwd=%q title=%q", cwd, title)
	}

	// the tail chunk starts right after a newline: its first line is whole
	titleLine := `{"type":"custom-title","customTitle":"exact"}` + "\n"
	fill := `{"type":"x","fill":"` + strings.Repeat("z", sessionChunk-len(titleLine)-len(`{"type":"x","fill":""}`+"\n")) + `"}` + "\n"
	pad := `{"type":"x","pad":"` + strings.Repeat("y", 2*sessionChunk) + `"}` + "\n" // file bigger than head+tail
	p = filepath.Join(d, "d.jsonl")
	os.WriteFile(p, []byte(first+"\n"+pad+titleLine+fill), 0o644)
	if len(titleLine+fill) != sessionChunk {
		t.Fatal("bad fixture")
	}
	if cwd, title := readSession(p); cwd != "/w" || title != "exact" {
		t.Errorf("boundary: cwd=%q title=%q", cwd, title)
	}
}

func TestFindSessionsOrderAndCap(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	w := filepath.Join(home, "w")
	mkdirs(t, w)
	claude := filepath.Join(home, ".claude")
	now := time.Now()
	writeSession(t, claude, w, "mid", now.Add(-2*time.Hour), userRec(t, w, "mid"))
	writeSession(t, claude, w, "new", now.Add(-time.Minute), userRec(t, w, "new"))
	writeSession(t, claude, w, "old", now.Add(-48*time.Hour), userRec(t, w, "old"))
	list, cut := findSessions(&Config{}, []string{w})
	if got := sessionIDs(list); cut || !reflect.DeepEqual(got, []string{"new", "mid", "old"}) {
		t.Fatalf("order: %v cut=%v", got, cut)
	}

	for i := range maxSessionFiles {
		writeSession(t, claude, w, fmt.Sprintf("bulk%03d", i), now.Add(-time.Duration(100+i)*time.Hour), userRec(t, w, "bulk"))
	}
	list, cut = findSessions(&Config{}, []string{w})
	if !cut || len(list) != maxSessionFiles || list[0].ID != "new" || list[len(list)-1].ID != fmt.Sprintf("bulk%03d", maxSessionFiles-4) {
		t.Fatalf("cap: n=%d cut=%v last=%s", len(list), cut, list[len(list)-1].ID)
	}
	for i := 1; i < len(list); i++ {
		if list[i].When.After(list[i-1].When) {
			t.Fatalf("not newest first at %d", i)
		}
	}
}

func TestFindSessionsAccounts(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "ignored")) // accounts win over the env
	p := filepath.Join(home, "p")
	mkdirs(t, p, "alpha", "beta", "gamma")
	alpha, beta, gamma := filepath.Join(p, "alpha"), filepath.Join(p, "beta"), filepath.Join(p, "gamma")
	cfg := &Config{
		Accounts: []Account{{Name: "personal", ConfigDir: ""}, {Name: "work", ConfigDir: "~/.claude-work"},
			{Name: "work2", ConfigDir: filepath.Join(home, ".claude-work")}, {Name: "home2", ConfigDir: filepath.Join(home, ".claude")}},
		FolderAccounts: map[string]string{beta: "work2"},
	}
	for i := range cfg.Accounts {
		cfg.Accounts[i].ConfigDir = expandPath(cfg.Accounts[i].ConfigDir)
	}
	now := time.Now()
	work := filepath.Join(home, ".claude-work")
	writeSession(t, filepath.Join(home, ".claude"), alpha, "s-alpha", now, userRec(t, alpha, "a"))
	writeSession(t, work, beta, "s-beta", now.Add(-time.Minute), userRec(t, beta, "b"))
	writeSession(t, work, gamma, "s-gamma", now.Add(-2*time.Minute), userRec(t, gamma, "c"))
	writeSession(t, filepath.Join(home, "ignored"), alpha, "s-env", now, userRec(t, alpha, "env"))
	list, _ := findSessions(cfg, []string{p})
	var got []string
	for _, s := range list {
		got = append(got, s.ID+"="+s.Account)
	}
	if want := []string{"s-alpha=personal", "s-beta=work2", "s-gamma=work"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFindSessionsMovedFolder(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	p := filepath.Join(home, "p")
	mkdirs(t, p, "team/.hidden/app", "x/a-b", "x/a/b", "y/a-b", "y/a/b")
	claude := filepath.Join(home, ".claude")
	now := time.Now()
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	gone := func(s string) string { return filepath.Join(p, "old", s) }
	moved := filepath.Join(p, "team/.hidden/app") // nested two levels, one a dot dir
	writeSession(t, claude, moved, "s-moved", at(1), userRec(t, gone("app"), "moved"))
	writeSession(t, claude, filepath.Join(p, "x/a/b"), "s-ambig", at(2), userRec(t, gone("ab"), "ambiguous"))
	writeSession(t, claude, filepath.Join(p, "nowhere/z"), "s-nowhere", at(3), userRec(t, gone("z"), "no folder"))
	writeSession(t, claude, filepath.Join(p, "y/a-b"), "s-fast", at(4), userRec(t, filepath.Join(p, "y/a-b"), "fast path"))
	list, _ := findSessions(&Config{}, []string{p})
	got := map[string]string{}
	for _, s := range list {
		got[s.ID] = s.Dir
	}
	if got["s-moved"] != moved {
		t.Errorf("moved: %q", got["s-moved"])
	}
	if d := got["s-ambig"]; d != filepath.Join(p, "x/a/b") && d != filepath.Join(p, "x/a-b") {
		t.Errorf("ambiguous: %q", d)
	}
	if got["s-nowhere"] != gone("z") { // as before: the recorded cwd, shown as missing
		t.Errorf("nowhere: %q", got["s-nowhere"])
	}
	if got["s-fast"] != filepath.Join(p, "y/a-b") { // a walk would pick y/a/b first
		t.Errorf("fast path: %q", got["s-fast"])
	}

	// a hit through the real path is mapped back under the base as given
	if err := os.Symlink(p, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	real := realPath(p)
	writeSession(t, claude, filepath.Join(real, "team/.hidden/app"), "s-real", at(0), userRec(t, gone("r"), "real"))
	list, _ = findSessions(&Config{}, []string{filepath.Join(home, "link")})
	if len(list) == 0 || list[0].ID != "s-real" || list[0].Dir != filepath.Join(home, "link/team/.hidden/app") {
		t.Errorf("real path: %+v", list)
	}
}

func TestFindSessionsPlainIDs(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	w := filepath.Join(home, "w")
	mkdirs(t, w)
	claude := filepath.Join(home, ".claude")
	for _, id := range []string{"0b1c-2d_3E", "--dangerously-skip-permissions", "-r", ".hidden", "_x", "a b", "a.b", "é"} {
		writeSession(t, claude, w, id, time.Now(), userRec(t, w, id))
	}
	list, _ := findSessions(&Config{}, []string{w})
	if got := sessionIDs(list); !reflect.DeepEqual(got, []string{"0b1c-2d_3E"}) {
		t.Fatalf("ids = %q", got)
	}
}

func TestFindSessionsUncleanCwd(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	base := filepath.Join(home, "p/api")
	mkdirs(t, home, "p/api/x", "p/outside")
	claude := filepath.Join(home, ".claude")
	now := time.Now()
	escape := base + "/../outside"
	writeSession(t, claude, escape, "s-escape", now, userRec(t, escape, "escape"))
	writeSession(t, claude, base, "s-dotdot", now, userRec(t, base+"/x/..", "dot dot"))
	writeSession(t, claude, base, "s-rel", now, userRec(t, "p/api", "relative"))
	writeSession(t, claude, filepath.Join(base, "x"), "s-x", now, userRec(t, base+"/../api/x", "resolves"))
	list, _ := findSessions(&Config{}, []string{base})
	got := map[string]string{}
	for _, s := range list {
		got[s.ID] = s.Dir
	}
	// unusable cwds resolve through the project dir name, or are skipped
	want := map[string]string{"s-x": filepath.Join(base, "x"), "s-dotdot": base, "s-rel": base}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestFindSessionsRootBase(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	w := realPath(filepath.Join(home, "w"))
	mkdirs(t, w, "app")
	claude := filepath.Join(home, ".claude")
	now := time.Now()
	writeSession(t, claude, w, "s-fast", now, userRec(t, w, "fast"))
	writeSession(t, claude, filepath.Join(w, "app"), "s-moved", now.Add(-time.Minute), userRec(t, "/gone/app", "moved"))
	list, _ := findSessions(&Config{}, []string{"/"})
	got := map[string]string{}
	for _, s := range list {
		got[s.ID] = s.Dir
	}
	if len(got) != 2 || got["s-fast"] != w || got["s-moved"] != filepath.Join(w, "app") {
		t.Fatalf("got %v", got)
	}
}

func TestResolveProjectPrefersCwdName(t *testing.T) {
	p := realPath(t.TempDir())
	mkdirs(t, p, "x/a/b", "x/a-b")
	scope := []scopeBase{{p, p}}
	name := projectKey(filepath.Join(p, "x/a-b"))
	for hint, want := range map[string]string{"a-b": "x/a-b", "b": "x/a/b", "other": "x/a/b"} {
		if got, ok := resolveProject(name, hint, scope); !ok || got != filepath.Join(p, want) {
			t.Errorf("hint %q: %q", hint, got)
		}
	}
	if got, ok := resolveProject(projectKey(filepath.Join(p, "x/nope")), "nope", scope); ok {
		t.Errorf("nothing there: %q", got)
	}
}
