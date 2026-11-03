//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var version = "dev"

const usage = `clauder - pick a folder and run claude there

Usage: clauder [clauder flags] [--] [claude args...]

Flags (long only; the first unknown argument and everything after it go to claude):
  --find QUERY      fuzzy-find a folder; one match runs directly, several open the picker
  --print-path      print the chosen path to stdout instead of running claude
  --favs            start in the favorites view
  --settings        start in the settings view (manage roots)
  --pull [PATH]     git pull --ff-only every repo under PATH (default: current dir)
  --clone-missing   with --pull: clone repos listed in .meta files that are missing
  --jobs N          with --pull: parallel git jobs (default: git.jobs from config)
  --config PATH     config file (default: $CLAUDER_CONFIG, $XDG_CONFIG_HOME/clauder/config.json,
                    ~/.config/clauder/config.json)
  --edit-config     open the config in $EDITOR (default vi)
  --help            show this help
  --version         show the version

Examples:
  clauder                      pick a folder, run claude
  clauder --resume             pick a folder, run claude --resume
  clauder --find infra -- -p "fix the bug"
  cd "$(clauder --print-path)"
`

type options struct {
	find               string
	hasFind            bool
	printPath, favs    bool
	settings           bool
	editConfig         bool
	help, version      bool
	config             string
	pull, cloneMissing bool
	pullPath           string
	jobs               int
	pass               []string
}

// parseArgs recognises clauder's own long flags. At "--" or the first
// unrecognised argument, everything from there on is passed to claude.
func parseArgs(args []string) (options, error) {
	var o options
	value := func(i *int, a, name string) (string, error) {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		var err error
		switch name {
		case "--":
			o.pass = args[i+1:]
			return o, nil
		case "--find":
			o.hasFind = true
			o.find, err = value(&i, a, name)
		case "--config":
			o.config, err = value(&i, a, name)
		case "--jobs":
			var v string
			if v, err = value(&i, a, name); err == nil {
				if o.jobs, err = strconv.Atoi(v); err != nil || o.jobs < 1 {
					err = fmt.Errorf("--jobs: invalid number %q", v)
				}
			}
		case "--pull":
			o.pull = true
			if v, ok := strings.CutPrefix(a, "--pull="); ok {
				o.pullPath = v
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				o.pullPath = args[i]
			}
		default:
			switch a {
			case "--print-path":
				o.printPath = true
			case "--favs":
				o.favs = true
			case "--settings":
				o.settings = true
			case "--clone-missing":
				o.cloneMissing = true
			case "--edit-config":
				o.editConfig = true
			case "--help":
				o.help = true
			case "--version":
				o.version = true
			default:
				o.pass = args[i:]
				return o, nil
			}
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

func main() { os.Exit(run(os.Args[1:])) }

// appVersion is the ldflags version, else the module version that
// "go install …@vX.Y.Z" records.
func appVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func run(args []string) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 2
	}
	if o.help {
		fmt.Print(usage)
		return 0
	}
	if o.version {
		fmt.Println("clauder", appVersion())
		return 0
	}
	path, err := configPath(o.config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 1
	}
	if o.editConfig { // before loading, so a broken config can be fixed
		if !fileExists(path) {
			if _, _, err := loadConfig(path); err != nil {
				fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
				return 1
			}
		}
		return editConfig(path)
	}
	cfg, created, err := loadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 1
	}
	if created {
		fmt.Fprintln(os.Stderr, dim("clauder: created default config at "+safePath(path)))
	}
	if o.pull {
		return runPull(cfg, o)
	}

	start := startOpts{}
	switch {
	case o.hasFind:
		entries := collect(walk(context.Background(), rootBases(cfg), cfg.Search.MaxDepth, cfg.Search.Ignore))
		res := rank(entries, o.find, favSet(cfg))
		switch len(res) {
		case 0:
			fmt.Fprintf(os.Stderr, "clauder: no folder matches %q\n", o.find)
			return 1
		case 1:
			return act(cfg, o, res[0].Path)
		}
		start = startOpts{mode: modeSearch, query: o.find, entries: entries}
	case o.favs:
		start.mode = modeFavs
	case o.settings:
		start.mode = modeSettings
	case created:
		start = startOpts{mode: modeSettings, msg: "add your project folders with a"}
	}

	chosen, code := runTUI(cfg, start)
	if code != 0 {
		return code
	}
	return act(cfg, o, chosen)
}

// runTUI renders on /dev/tty (falling back to stdin/stderr) so stdout stays
// free for --print-path.
func runTUI(cfg *Config, start startOpts) (string, int) {
	var in io.Reader = os.Stdin
	var out io.Writer = os.Stderr
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		defer tty.Close()
		in, out = tty, tty
	}
	m := newModel(cfg, lipgloss.NewRenderer(out), start)
	final, err := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out)).Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
		return "", 130
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return "", 1
	}
	fm := final.(*model)
	fm.stopJobs()
	fm.waitJobs(10 * time.Second)
	if fm.code == 1 {
		fmt.Fprintln(os.Stderr, dim("cancelled"))
	}
	return fm.chosen, fm.code
}

// act prints the path (--print-path) or execs the command in it.
func act(cfg *Config, o options, dir string) int {
	if o.printPath {
		fmt.Println(dir)
		return 0
	}
	fmt.Fprintln(os.Stderr, dim("→ "+sanitize(dir, false)))
	bin, err := exec.LookPath(cfg.Command)
	if err == nil {
		bin, err = filepath.Abs(bin) // a relative PATH hit must survive the chdir
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 127
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 1
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "PWD=") })
	env = append(env, "PWD="+dir)
	err = syscall.Exec(bin, append([]string{cfg.Command}, o.pass...), env)
	fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
	return 1
}

func editConfig(path string) int {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 1
	}
	return 0
}

func runPull(cfg *Config, o options) int {
	target := "."
	if o.pullPath != "" {
		target = expandPath(o.pullPath)
	}
	target, _ = filepath.Abs(target)
	repos, err := discover(target, cfg.Search.Ignore, cfg.Git.Exclude)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauder:", sanitize(err.Error(), false))
		return 1
	}
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "clauder: no git repositories under", safePath(target))
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobs := cfg.Git.Jobs
	if o.jobs > 0 {
		jobs = o.jobs
	}
	color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	line := func(r RepoResult) string {
		l := statusIcon(r.Status) + " " + sanitize(r.Rel, false)
		switch r.Status {
		case StFailed:
			l = paint("31", l) + ": " + sanitize(r.Err, false)
		case StUpdated, StCloned:
			l = paint("32", l)
		case StMissing:
			l = paint("33", l) + " (missing)"
		}
		if r.Status == StCloned {
			l += " (cloned)"
		}
		if r.Dirty {
			l += " " + paint("33", "⚠ dirty")
		}
		return l
	}
	list := repos
	byDir := map[string]RepoResult{}
	printed := map[string]bool{}
	began := time.Now()
	opts := SyncOpts{Jobs: jobs, Timeout: cfg.GitTimeout(), Clone: o.cloneMissing,
		Rediscover: func() ([]Repo, error) { return discover(target, cfg.Search.Ignore, cfg.Git.Exclude) }}
	for ev := range runSync(ctx, repos, opts) {
		switch r := ev.Result; {
		case ev.Note != "":
			fmt.Fprintln(os.Stderr, dim(sanitize(ev.Note, false)))
		case ev.Repos != nil:
			list = ev.Repos
		case r.Status == StRunning || r.Status == StPending:
		case r.Status == StFailed && authTransient(r):
			byDir[r.Dir] = r // retried after the pool; printed then (or at the end)
		default:
			byDir[r.Dir] = r
			if !printed[r.Dir] {
				printed[r.Dir] = true
				fmt.Println(line(r))
			}
		}
	}
	results := make([]RepoResult, 0, len(list))
	for _, r := range list {
		res, ok := byDir[r.Dir]
		if !ok {
			continue
		}
		if !printed[r.Dir] {
			fmt.Println(line(res))
		}
		results = append(results, res)
	}
	fmt.Printf("%s (%s)\n", summarize(results), time.Since(began).Round(100*time.Millisecond))
	if ctx.Err() != nil {
		return 130
	}
	for _, r := range results {
		if r.Status == StFailed {
			return 1
		}
	}
	return 0
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// dim styles stderr notices unless NO_COLOR is set or stderr is not a terminal.
func dim(s string) string {
	if os.Getenv("NO_COLOR") != "" || !isTerminal(os.Stderr) {
		return s
	}
	return "\x1b[38;5;242m" + s + "\x1b[0m"
}
