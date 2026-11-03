//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func mkdirs(t *testing.T, base string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(base, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDefaultConfigCreated(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, ".config", "clauder", "config.json")
	cfg, created, err := loadConfig(path)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if len(cfg.Roots) != 1 || cfg.Roots[0].Name != "home" || cfg.Roots[0].Path != home {
		t.Fatalf("roots = %+v", cfg.Roots)
	}
	if cfg.Command != "claude" || cfg.Search.MaxDepth != 4 || cfg.Git.Jobs != 8 || cfg.GitTimeout().Seconds() != 120 {
		t.Fatalf("defaults not filled: %+v", cfg)
	}
	m := readJSON(t, path)
	roots := m["roots"].([]any)
	if roots[0].(map[string]any)["path"] != "~" {
		t.Fatalf("root stored as %v, want ~", roots[0])
	}
	if _, created, _ = loadConfig(path); created {
		t.Fatal("second load must not recreate")
	}
}

func TestLoadSaveTilde(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "cfg.json")
	src := `{"roots":[{"name":"w","path":"~/work","color":99},{"name":"abs","path":"/opt/x"}],"favorites":["~/work/a"]}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, created, err := loadConfig(path)
	if err != nil || created {
		t.Fatal(err, created)
	}
	if cfg.Roots[0].Path != filepath.Join(home, "work") || cfg.Favorites[0] != filepath.Join(home, "work", "a") {
		t.Fatalf("not expanded: %+v %v", cfg.Roots, cfg.Favorites)
	}
	if cfg.RootColor(0) != 99 || cfg.RootColor(1) != palette[1] {
		t.Fatalf("colours %d %d", cfg.RootColor(0), cfg.RootColor(1))
	}
	if _, err := cfg.ToggleFavorite(filepath.Join(home, "other")); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, path)
	if got := m["favorites"]; !reflect.DeepEqual(got, []any{"~/work/a", "~/other"}) {
		t.Fatalf("favorites saved as %v", got)
	}
	roots := m["roots"].([]any)
	if roots[0].(map[string]any)["path"] != "~/work" || roots[1].(map[string]any)["path"] != "/opt/x" {
		t.Fatalf("roots saved as %v", roots)
	}
	if _, ok := roots[1].(map[string]any)["color"]; ok {
		t.Fatal("auto colour must not be persisted")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "\n  \"roots\"") {
		t.Fatal("config not indented")
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLoadBadTimeout(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "cfg.json")
	os.WriteFile(path, []byte(`{"git":{"timeout":"soon"}}`), 0o644)
	if _, _, err := loadConfig(path); err == nil {
		t.Fatal("want error for bad git.timeout")
	}
}

func TestExpandAndTilde(t *testing.T) {
	home := setHome(t)
	cases := map[string]string{"~": home, "~/a/b": home + "/a/b", "/x/../y": "/y", "rel": "rel"}
	for in, want := range cases {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
	if tildePath(home+"/a") != "~/a" || tildePath(home) != "~" || tildePath("/etc") != "/etc" || tildePath(home+"x") != home+"x" {
		t.Error("tildePath wrong")
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	home := setHome(t)
	t.Setenv("CLAUDER_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	check := func(flag, want string) {
		t.Helper()
		got, err := configPath(flag)
		if err != nil || got != want {
			t.Errorf("configPath(%q) = %q, %v; want %q", flag, got, err, want)
		}
	}
	check("", filepath.Join(home, ".config/clauder/config.json"))
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	check("", "/xdg/clauder/config.json")
	t.Setenv("CLAUDER_CONFIG", "~/env.json")
	check("", filepath.Join(home, "env.json"))
	check("/flag.json", "/flag.json")
}

func TestLocateAndLabel(t *testing.T) {
	home := setHome(t)
	cfg := &Config{Roots: []Root{{Name: "a", Path: "/w"}, {Name: "b", Path: "/w/sub"}}}
	if i, rel := cfg.locate("/w/sub/x/y"); i != 1 || rel != "x/y" {
		t.Errorf("deepest root not chosen: %d %q", i, rel)
	}
	if cfg.label("/w") != "a" || cfg.label("/w/other") != "a/other" || cfg.label("/wx") != "/wx" {
		t.Error("label wrong")
	}
	if cfg.label(home+"/p") != "~/p" {
		t.Error("label outside roots should be ~/...")
	}
	if cfg.uniqueName("a") != "a-2" || cfg.uniqueName("c") != "c" {
		t.Error("uniqueName wrong")
	}
}

func TestSaveThroughSymlinkKeepsUnknownKeysAndMode(t *testing.T) {
	home := setHome(t)
	real := filepath.Join(home, "dotfiles", "config.json")
	writeFile(t, real, `{"theme":"dark","roots":[{"name":"w","path":"~/w"}],"zeta":{"a":[1,2]}}`)
	os.Chmod(real, 0o600)
	link := filepath.Join(home, ".config", "clauder", "config.json")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ToggleFavorite(filepath.Join(home, "w")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	st, _ := os.Stat(real)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	m := readJSON(t, real)
	if m["theme"] != "dark" || m["zeta"] == nil || !reflect.DeepEqual(m["favorites"], []any{"~/w"}) {
		t.Fatalf("saved = %v", m)
	}
}

func TestEditConfigWithBrokenConfig(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "cfg.json")
	writeFile(t, path, "{broken")
	t.Setenv("EDITOR", "true")
	if code := run([]string{"--config", path, "--edit-config"}); code != 0 {
		t.Fatalf("--edit-config exit %d", code)
	}
	if code := run([]string{"--config", path, "--print-path"}); code != 1 {
		t.Fatalf("broken config should fail normal runs, got %d", code)
	}
}
