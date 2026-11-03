//go:build unix

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
)

func TestMain(m *testing.M) {
	initStyles(lipgloss.NewRenderer(io.Discard)) // plain text output
	os.Exit(m.Run())
}

// fixture: $HOME/proj with alpha/{one,two}, beta, gamma/deep and one root "work".
func fixture(t *testing.T) (*Config, string) {
	t.Helper()
	home := setHome(t)
	root := filepath.Join(home, "proj")
	mkdirs(t, root, "alpha/one", "alpha/two", "beta", "gamma/deep", ".hidden/x", "@eaDir/y", "#recycle/z")
	cfg, _, err := loadConfig(filepath.Join(home, "cfg.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roots = []Root{{Name: "work", Path: root}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfg, root
}

func start(t *testing.T, cfg *Config, s startOpts) *teatest.TestModel {
	t.Helper()
	return teatest.NewTestModel(t, newModel(cfg, nil, s), teatest.WithInitialTermSize(100, 30))
}

func finalModel(t *testing.T, tm *teatest.TestModel) *model {
	t.Helper()
	return tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*model)
}

func waitFor(t *testing.T, tm *teatest.TestModel, s string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
		teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func reload(t *testing.T, cfg *Config) *Config {
	t.Helper()
	c, _, err := loadConfig(cfg.file)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTUINavigateUp(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Send(key(tea.KeyRight)) // open work
	tm.Send(key(tea.KeyRight)) // open alpha
	waitFor(t, tm, "work/alpha")
	tm.Send(key(tea.KeyLeft)) // back to work, alpha highlighted
	tm.Type("j")              // beta
	tm.Send(key(tea.KeyEnter))
	if m := finalModel(t, tm); m.code != 0 || m.chosen != filepath.Join(root, "beta") {
		t.Fatalf("code=%d chosen=%q", m.code, m.chosen)
	}
}

func TestTUIQuickPickRunsLeafAndDot(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Type("1")  // open work
	tm.Type("1")  // open alpha
	tm.Type("kk") // wraps: one -> two -> one
	tm.Type(".")
	if m := finalModel(t, tm); m.chosen != filepath.Join(root, "alpha") {
		t.Fatalf("chosen=%q", m.chosen)
	}
}

func TestTUICancel(t *testing.T) {
	cfg, _ := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Type("q")
	if m := finalModel(t, tm); m.code != 1 || m.chosen != "" || m.View() != "" {
		t.Fatalf("code=%d chosen=%q", m.code, m.chosen)
	}
	tm = start(t, cfg, startOpts{})
	tm.Send(key(tea.KeyCtrlC))
	if m := finalModel(t, tm); m.code != 130 {
		t.Fatalf("ctrl-c code=%d", m.code)
	}
}

func TestTUISearchRun(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Type("/")
	tm.Type("deep")
	waitFor(t, tm, "❯ work/gamma/deep")
	tm.Send(key(tea.KeyEnter))
	if m := finalModel(t, tm); m.chosen != filepath.Join(root, "gamma/deep") {
		t.Fatalf("chosen=%q", m.chosen)
	}
}

func TestTUISearchOpenInBrowse(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Type("/alpha")
	waitFor(t, tm, "❯ work/alpha")
	tm.Send(key(tea.KeyRight)) // browse alpha
	waitFor(t, tm, "one")
	tm.Type("j")
	tm.Send(key(tea.KeyEnter))
	if m := finalModel(t, tm); m.chosen != filepath.Join(root, "alpha/two") {
		t.Fatalf("chosen=%q", m.chosen)
	}
}

func TestTUINewFolder(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Type("n")
	waitFor(t, tm, "open a folder first")
	tm.Send(key(tea.KeyRight))
	tm.Type("nalpha")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "alpha already exists")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("fresh")
	tm.Send(key(tea.KeyTab)) // create and open
	waitFor(t, tm, "work/fresh")
	tm.Type("n")
	tm.Type("inner")
	tm.Send(key(tea.KeyEnter)) // create and run
	m := finalModel(t, tm)
	want := filepath.Join(root, "fresh/inner")
	if m.chosen != want || !isDir(want) {
		t.Fatalf("chosen=%q", m.chosen)
	}
}

func TestTUIFavoriteToggle(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Send(key(tea.KeyRight))
	tm.Type("f")                // favorite alpha
	waitFor(t, tm, "alpha › ★") // marker on the row
	tm.Send(key(tea.KeyLeft))
	waitFor(t, tm, "1) ★ favorites ›") // pseudo-entry, no duplicate rows
	tm.Type("k")                       // wrap to the pseudo-entry
	tm.Send(key(tea.KeyRight))         // open the favorites list
	waitFor(t, tm, "1) work/alpha")
	tm.Send(key(tea.KeyEsc)) // back: pseudo-entry highlighted
	waitFor(t, tm, "❯ 1) ★ favorites")
	tm.Send(key(tea.KeyEnter)) // ⏎ on it opens the list too
	waitFor(t, tm, "f remove")
	tm.Send(key(tea.KeyEnter)) // ⏎ runs the favorite
	m := finalModel(t, tm)
	if m.chosen != filepath.Join(root, "alpha") {
		t.Fatalf("chosen = %q", m.chosen)
	}
	if got := reload(t, cfg).Favorites; !reflect.DeepEqual(got, []string{filepath.Join(root, "alpha")}) {
		t.Fatalf("favorites = %v", got)
	}

	// remove from the list, including a missing one; the pseudo-entry disappears
	cfg = reload(t, cfg)
	cfg.Favorites = append(cfg.Favorites, filepath.Join(root, "gone"))
	cfg.Save()
	tm = start(t, cfg, startOpts{})
	tm.Type("1")
	waitFor(t, tm, "(missing)")
	tm.Type("f")
	tm.Type("f")
	waitFor(t, tm, "no favorites yet")
	tm.Send(key(tea.KeyLeft))
	waitFor(t, tm, "❯ 1) work")
	tm.Type("q")
	finalModel(t, tm)
	if got := reload(t, cfg).Favorites; len(got) != 0 {
		t.Fatalf("favorites = %v", got)
	}
}

func TestTUIFavsFromFolderReturnsThere(t *testing.T) {
	cfg, root := fixture(t)
	cfg.Favorites = []string{filepath.Join(root, "beta")}
	cfg.Save()
	tm := start(t, cfg, startOpts{})
	tm.Type("2")  // open work (row 1 is the pseudo-entry)
	tm.Type("jj") // gamma
	tm.Type("F")  // favorites list
	waitFor(t, tm, "work/beta")
	tm.Send(key(tea.KeyEsc))
	tm.Send(key(tea.KeyEnter)) // gamma is still highlighted
	if m := finalModel(t, tm); m.chosen != filepath.Join(root, "gamma") {
		t.Fatalf("chosen = %q", m.chosen)
	}
}

func TestTUIPlusAddsRoot(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{})
	tm.Send(key(tea.KeyRight))
	tm.Type("+")
	waitFor(t, tm, "added root alpha")
	tm.Type("q")
	finalModel(t, tm)
	got := reload(t, cfg).Roots
	if len(got) != 2 || got[1].Name != "alpha" || got[1].Path != filepath.Join(root, "alpha") || got[1].Color == 0 {
		t.Fatalf("roots = %+v", got)
	}
}

func TestTUISettingsAdd(t *testing.T) {
	cfg, root := fixture(t)
	tm := start(t, cfg, startOpts{mode: modeSettings, msg: "add your project folders with a"})
	waitFor(t, tm, "add your project folders with a")
	tm.Type("a")
	tm.Type("~/pr")
	tm.Send(key(tea.KeyTab)) // -> ~/proj/
	tm.Type("g")
	tm.Send(key(tea.KeyTab)) // -> ~/proj/gamma/
	waitFor(t, tm, "~/proj/gamma/")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "name: gamma")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "added root gamma")
	tm.Send(key(tea.KeyEsc)) // top screen
	waitFor(t, tm, "clauder")
	tm.Type("q")
	finalModel(t, tm)
	got := reload(t, cfg).Roots
	if len(got) != 2 || got[1].Name != "gamma" || got[1].Path != filepath.Join(root, "gamma") {
		t.Fatalf("roots = %+v", got)
	}
	data, _ := os.ReadFile(cfg.file)
	if !bytes.Contains(data, []byte(`"path": "~/proj/gamma"`)) {
		t.Fatalf("not saved with ~:\n%s", data)
	}
}

func TestTUISettingsAddValidation(t *testing.T) {
	cfg, _ := fixture(t)
	tm := start(t, cfg, startOpts{mode: modeSettings})
	tm.Type("a~/nope")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "is not a directory")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("~/proj")
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "already a root")
	tm.Send(key(tea.KeyEsc))
	tm.Send(key(tea.KeyEsc))
	tm.Type("q")
	if got := reload(t, finalModel(t, tm).cfg).Roots; len(got) != 1 {
		t.Fatalf("roots = %+v", got)
	}
}

func TestCompleteDir(t *testing.T) {
	home := setHome(t)
	mkdirs(t, home, "proj/alpha", "proj/alpine", "proj/beta", "proj/.dot")
	v, c, _ := completeDir("~/proj/al", nil)
	if v != "~/proj/alp" || c != nil {
		t.Fatalf("lcp: %q", v)
	}
	v, c, cands := completeDir(v, nil) // no progress -> cycle
	if v != "~/proj/alpha" || c == nil || len(cands) != 2 {
		t.Fatalf("cycle start: %q %v", v, cands)
	}
	if v, c, _ = completeDir(v, c); v != "~/proj/alpine" {
		t.Fatalf("cycle next: %q", v)
	}
	if v, _, _ = completeDir(v, c); v != "~/proj/alpha" {
		t.Fatalf("cycle wrap: %q", v)
	}
	if v, _, _ = completeDir("~/proj/b", nil); v != "~/proj/beta/" {
		t.Fatalf("single: %q", v)
	}
	if v, _, _ = completeDir("~/proj/zz", nil); v != "~/proj/zz" {
		t.Fatalf("none: %q", v)
	}
}

func TestTUISettingsRenameDeleteReorder(t *testing.T) {
	cfg, root := fixture(t)
	cfg.Roots = append(cfg.Roots, Root{Name: "second", Path: filepath.Join(root, "beta")}, Root{Name: "third", Path: "/does/not/exist"})
	cfg.Save()
	tm := start(t, cfg, startOpts{mode: modeSettings})
	waitFor(t, tm, "(missing)")
	tm.Type("r")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("second") // duplicate
	tm.Send(key(tea.KeyEnter))
	waitFor(t, tm, "already exists")
	tm.Send(key(tea.KeyCtrlU))
	tm.Type("main")
	tm.Send(key(tea.KeyEnter))
	tm.Type("J") // main moves below second
	tm.Type("j") // select third
	tm.Type("d")
	waitFor(t, tm, "delete root third? y/n")
	tm.Type("n") // cancelled
	tm.Type("x")
	tm.Type("y")
	waitFor(t, tm, "deleted root third")
	tm.Type("q")
	tm.Type("q")
	finalModel(t, tm)
	got := reload(t, cfg).Roots
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"second", "main"}) {
		t.Fatalf("roots = %v", names)
	}
	if got[1].Color != palette[0] || got[0].Color != palette[1] {
		t.Fatalf("colours should follow the roots when reordered: %+v", got)
	}
}

func TestTUISyncView(t *testing.T) {
	isolateGit(t)
	cfg, root := fixture(t)
	d := t.TempDir()
	bare, seed := remoteFixture(t, d)
	gitT(t, d, "clone", "-q", bare, filepath.Join(root, "repo1"))
	pushCommit(t, seed, "2")
	tm := start(t, cfg, startOpts{})
	tm.Type("p") // sync the "work" root: fallback finds child repo repo1
	waitFor(t, tm, "1 updated · 0 up to date · 0 failed · 0 dirty · 0 missing")
	tm.Send(key(tea.KeyEnter)) // show git output
	waitFor(t, tm, "Fast-forward")
	tm.Send(key(tea.KeyEsc))
	tm.Send(key(tea.KeyEsc)) // back to browse
	waitFor(t, tm, "s settings")
	tm.Type("q")
	m := finalModel(t, tm)
	if m.code != 1 || len(m.sync.results) != 1 || m.sync.results[0].Status != StUpdated {
		t.Fatalf("code=%d results=%+v", m.code, m.sync.results)
	}
}

func TestValidateNewFolder(t *testing.T) {
	d := t.TempDir()
	mkdirs(t, d, "taken")
	for name, ok := range map[string]bool{"good": true, "": false, " lead": false, "trail ": false, "a/b": false,
		".": false, "..": false, ".hidden": false, "@x": false, "#x": false, "taken": false} {
		if err := validateNewFolder(d, name); (err == nil) != ok {
			t.Errorf("%q: err=%v", name, err)
		}
	}
}

func TestRowsShortTerminal(t *testing.T) {
	m := &model{height: 6}
	if got := m.rows(50, 4); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
	m.width, m.height = 200, 30
	if got := m.rows(50, 0); got != 23 {
		t.Errorf("rows = %d, want 23", got)
	}
}

func TestFooterWrapsAtWidth(t *testing.T) {
	for _, w := range []int{80, 40} {
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			cfg, root := fixture(t)
			for i := range 30 {
				mkdirs(t, root, fmt.Sprintf("many/d%02d", i))
			}
			tm := teatest.NewTestModel(t, newModel(cfg, nil, startOpts{}), teatest.WithInitialTermSize(w, 30))
			tm.Send(key(tea.KeyRight)) // into work: the longest hint
			teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
				return bytes.Contains(b, []byte(". here")) && bytes.Contains(b, []byte("p pull · q quit"))
			}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
			tm.Type("q")
			finalModel(t, tm)

			// every line fits, and the list shrinks for the extra hint lines
			m := newModel(cfg, nil, startOpts{})
			m.Update(tea.WindowSizeMsg{Width: w, Height: 14})
			m.enter(filepath.Join(root, "many"), false)
			lines := strings.Split(m.View(), "\n")
			if len(lines) > 14 {
				t.Errorf("view has %d lines, terminal 14:\n%s", len(lines), m.View())
			}
			if !strings.Contains(lines[1], "work/many") {
				t.Errorf("header missing: %q", lines[1])
			}
			for _, l := range lines {
				if lipgloss.Width(l) > w {
					t.Errorf("line wider than %d: %q", w, l)
				}
			}
			if !strings.HasSuffix(lines[len(lines)-1], "q quit") || len(m.hintLines()) < 2 && w == 40 {
				t.Errorf("footer not wrapped: %q", lines[len(lines)-2:])
			}
		})
	}
}

func TestWrapHint(t *testing.T) {
	got := wrapHint("aa · bb · cc · dd", 9)
	if want := []string{"aa · bb", "cc · dd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapHint = %q", got)
	}
}
