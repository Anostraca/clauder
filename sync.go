//go:build unix

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Repo is one git repository to sync.
type Repo struct {
	Dir  string // absolute
	Rel  string // relative to the sync target
	URL  string // clone URL from a .meta file, may be empty
	Meta bool   // repo holds .meta files: pulled before the others
	Bad  string // invalid .meta entry, reported as a failed row
}

const metaDepth = 4

// discover walks target (up to metaDepth levels) and decides per directory:
//   - a regular file .meta adds its listed projects (and the dir itself, if
//     it is a repo, as a meta repo that is pulled first);
//   - otherwise a .git entry makes the dir a plain repo, not descended into;
//   - otherwise it descends.
//
// The target itself is always descended. exclude globs are matched against
// the path relative to target.
func discover(target string, ignore, exclude []string) ([]Repo, error) {
	if !isDir(target) {
		return nil, fmt.Errorf("%s: not a directory", target)
	}
	projects := map[string]Repo{}
	claimed := map[string]bool{}
	anyMeta := false
	add := func(r Repo) {
		if old, ok := projects[r.Dir]; ok {
			r.URL = cmp.Or(r.URL, old.URL)
			r.Meta = r.Meta || old.Meta
		}
		projects[r.Dir] = r
	}
	relTo := func(p string) string {
		rel, err := filepath.Rel(target, p)
		if err != nil {
			return p
		}
		return filepath.ToSlash(rel)
	}

	readMeta := func(dir string) {
		file := filepath.Join(dir, ".meta")
		var m struct {
			Projects map[string]string `json:"projects"`
		}
		data, err := os.ReadFile(file)
		if err == nil {
			err = json.Unmarshal(data, &m)
		}
		if err != nil {
			add(Repo{Dir: file, Rel: relTo(file), Bad: "cannot read .meta: " + err.Error()})
			return
		}
		for key, u := range m.Projects {
			p, err := metaProjectPath(dir, key)
			if err != nil {
				id := file + "\x00" + key
				add(Repo{Dir: id, Rel: relTo(file) + ": " + key, Bad: fmt.Sprintf("invalid project path %q: %v", key, err)})
				continue
			}
			add(Repo{Dir: p, URL: u})
			claimed[p] = true
		}
	}

	var walkDir func(dir string, depth int)
	walkDir = func(dir string, depth int) {
		repo := isRepo(dir)
		switch {
		case isRegularFile(filepath.Join(dir, ".meta")):
			anyMeta = true
			readMeta(dir)
			if repo {
				add(Repo{Dir: dir, Meta: true})
			}
		case repo:
			add(Repo{Dir: dir})
			if dir != target {
				return
			}
		case claimed[dir]:
			return // listed project that is not a repo: syncRepo reports it
		}
		if depth >= metaDepth {
			return
		}
		for _, name := range listDirs(dir) {
			if !ignored(name, ignore) {
				walkDir(filepath.Join(dir, name), depth+1)
			}
		}
	}
	walkDir(target, 0)
	if r, ok := projects[target]; ok && anyMeta {
		r.Meta = true
		projects[target] = r
	}

	var repos []Repo
	for _, r := range projects {
		if r.Rel == "" {
			r.Rel = relTo(r.Dir)
		}
		if excluded(r.Rel, exclude) {
			continue
		}
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Rel < repos[j].Rel })
	return repos, nil
}

// metaProjectPath resolves a .meta project key to a directory strictly inside
// dir, also after resolving symlinks of its nearest existing parent.
func metaProjectPath(dir, key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || filepath.IsAbs(key) {
		return "", errors.New("must be a relative path")
	}
	p := filepath.Join(dir, filepath.FromSlash(key))
	if !inside(dir, p, false) {
		return "", errors.New("points outside the .meta directory")
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	a := p
	for a != dir && !fileExists(a) {
		a = filepath.Dir(a)
	}
	realA, err := filepath.EvalSymlinks(a)
	if err != nil {
		return "", err
	}
	if !inside(realDir, realA, true) {
		return "", errors.New("resolves outside the .meta directory via a symlink")
	}
	return p, nil
}

// inside reports whether p is below base (or equal to it, if allowEqual).
func inside(base, p string, allowEqual bool) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return allowEqual || rel != "."
}

// excluded matches rel and each of its parent paths against the globs, so
// "archive" excludes everything below archive/ as well.
func excluded(rel string, globs []string) bool {
	for _, g := range globs {
		for p := rel; p != "." && p != ""; p = path.Dir(p) {
			if globMatch(g, p) {
				return true
			}
		}
	}
	return false
}

// globMatch is path.Match with "**" matching any number of segments.
func globMatch(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isRepo(dir string) bool { return fileExists(filepath.Join(dir, ".git")) }

type RepoStatus int

const (
	StPending RepoStatus = iota
	StRunning
	StUpdated
	StUpToDate
	StCloned
	StMissing
	StFailed
)

type RepoResult struct {
	Repo
	Status RepoStatus
	Dirty  bool
	Output string // combined git output
	Err    string // first meaningful stderr line on failure
}

type SyncOpts struct {
	Jobs    int
	Timeout time.Duration
	Clone   bool
	// Rediscover re-reads the repo list after the meta repos are pulled, so
	// projects added upstream show up (as missing). Optional.
	Rediscover func() ([]Repo, error)
}

// SyncEvent is a repo status update, a replaced repo list, or a progress note.
type SyncEvent struct {
	Index  int
	Result RepoResult
	Repos  []Repo // non-nil: the list was re-read; later indexes refer to it
	Note   string // non-empty: progress note such as "connecting to host…"
}

// repoInfo is what runSync learns about a repo before pulling.
type repoInfo struct {
	remote string // origin URL (existing repo) or .meta URL (missing)
	ownSSH bool   // core.sshCommand is set: leave its ssh alone
}

// runSync pulls (or clones) repos with a bounded worker pool: meta repos
// first, then (after Rediscover) the rest. Each repo gets a StRunning and a
// final event (again for a retried repo). The channel closes once ssh
// masters are shut down. After ctx is cancelled, events may be dropped.
//
// Unless the user set GIT_SSH, GIT_SSH_COMMAND or core.sshCommand, ssh
// connections are multiplexed: one master per ssh host is opened serially up
// front, so parallel jobs share a single authenticated connection (agents
// such as 1Password refuse many parallel signing requests). Failures that
// look like transient agent errors get one serial retry after the pool.
func runSync(ctx context.Context, repos []Repo, o SyncOpts) <-chan SyncEvent {
	if o.Jobs <= 0 {
		o.Jobs = 8
	}
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	out := make(chan SyncEvent, len(repos)*4+16)
	send := func(ev SyncEvent) {
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(out)
		globalSSH := os.Getenv("GIT_SSH") != "" || os.Getenv("GIT_SSH_COMMAND") != ""
		info := inspectRepos(ctx, repos, o.Jobs)
		sshCmd := "ssh -o BatchMode=yes"
		if !globalSSH {
			if mux, err := newSSHMux(); err == nil {
				defer mux.close()
				for _, t := range sshTargets(repos, info) {
					send(SyncEvent{Note: "connecting to " + t.host + "…"})
					mux.prewarm(ctx, t)
				}
				sshCmd = mux.command()
			}
		}
		envFor := func(r Repo) []string {
			env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if globalSSH || info[r.Dir].ownSSH {
				return env
			}
			return append(env, "GIT_SSH_COMMAND="+sshCmd)
		}

		cur := repos
		results := make([]RepoResult, len(cur))
		done := make([]bool, len(cur))
		run := func(i int) {
			send(SyncEvent{Index: i, Result: RepoResult{Repo: cur[i], Status: StRunning}})
			results[i] = syncRepo(ctx, cur[i], o, envFor(cur[i]))
			done[i] = true
			send(SyncEvent{Index: i, Result: results[i]})
		}
		pool := func(idx []int) {
			jobs := make(chan int)
			var wg sync.WaitGroup
			for range min(o.Jobs, max(len(idx), 1)) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range jobs {
						run(i)
					}
				}()
			}
			for _, i := range idx {
				jobs <- i
			}
			close(jobs)
			wg.Wait()
		}

		var meta []int
		for i, r := range cur {
			if r.Meta {
				meta = append(meta, i)
			}
		}
		if len(meta) > 0 {
			pool(meta)
			if o.Rediscover != nil && ctx.Err() == nil {
				if next, err := o.Rediscover(); err == nil {
					prev := map[string]RepoResult{}
					for _, i := range meta {
						prev[cur[i].Dir] = results[i]
					}
					var fresh []Repo
					for _, r := range next {
						if _, ok := info[r.Dir]; !ok {
							fresh = append(fresh, r)
						}
					}
					for dir, in := range inspectRepos(ctx, fresh, o.Jobs) {
						info[dir] = in
					}
					cur = next
					results, done = make([]RepoResult, len(cur)), make([]bool, len(cur))
					send(SyncEvent{Repos: cur})
					for i, r := range cur {
						if res, ok := prev[r.Dir]; ok {
							results[i], done[i] = res, true
							send(SyncEvent{Index: i, Result: res})
						}
					}
				}
			}
		}
		var rest []int
		for i := range cur {
			if !done[i] {
				rest = append(rest, i)
			}
		}
		pool(rest)

		for i := range cur {
			if ctx.Err() == nil && authTransient(results[i]) {
				run(i)
			}
		}
	}()
	return out
}

// inspectRepos reads each repo's remote and core.sshCommand in parallel.
func inspectRepos(ctx context.Context, repos []Repo, jobs int) map[string]repoInfo {
	info := make(map[string]repoInfo, len(repos))
	var mu sync.Mutex
	ch := make(chan Repo)
	var wg sync.WaitGroup
	for range min(jobs, max(len(repos), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range ch {
				var in repoInfo
				if r.Bad == "" {
					dir := r.Dir
					if isRepo(dir) {
						out, _, _ := git(ctx, nil, dir, "remote", "get-url", "origin")
						in.remote = strings.TrimSpace(out)
					} else {
						in.remote = r.URL
						for dir != "/" && !isDir(dir) {
							dir = filepath.Dir(dir)
						}
					}
					out, _, _ := git(ctx, nil, dir, "config", "--get", "core.sshCommand")
					in.ownSSH = strings.TrimSpace(out) != ""
				}
				mu.Lock()
				info[r.Dir] = in
				mu.Unlock()
			}
		}()
	}
	for _, r := range repos {
		ch <- r
	}
	close(ch)
	wg.Wait()
	return info
}

var transientAuth = []string{"agent refused operation", "communication with agent failed", "sign_and_send_pubkey"}

// authTransient reports a failure caused by the ssh agent rather than the repo.
func authTransient(r RepoResult) bool {
	if r.Status != StFailed || r.Bad != "" {
		return false
	}
	for _, p := range transientAuth {
		if strings.Contains(r.Output, p) || strings.Contains(r.Err, p) {
			return true
		}
	}
	return false
}

type sshTarget struct{ user, host, port string }

// args returns ssh arguments for the destination, after a "--".
func (t sshTarget) args() []string {
	dest := t.host
	if t.user != "" {
		dest = t.user + "@" + t.host
	}
	if t.port != "" {
		return []string{"-p", t.port, "--", dest}
	}
	return []string{"--", dest}
}

// parseSSHRemote extracts the ssh destination from a git remote URL:
// "[user@]host:path" or "ssh://[user@]host[:port]/path". Other schemes
// (https, git, file), local paths and hosts or users starting with "-" are
// rejected. An empty user means ssh's default, as in git's own ssh call.
func parseSSHRemote(raw string) (sshTarget, bool) {
	var t sshTarget
	switch {
	case strings.HasPrefix(raw, "ssh://"), strings.HasPrefix(raw, "git+ssh://"), strings.HasPrefix(raw, "ssh+git://"):
		u, err := url.Parse("ssh://" + raw[strings.Index(raw, "://")+3:])
		if err != nil {
			return t, false
		}
		t = sshTarget{host: u.Hostname(), port: u.Port()}
		if u.User != nil {
			t.user = u.User.Username()
		}
	case strings.Contains(raw, "://"):
		return t, false
	default:
		i := strings.Index(raw, ":")
		if i <= 0 || strings.Contains(raw[:i], "/") {
			return t, false // local path
		}
		t.host = raw[:i]
		if k := strings.LastIndex(t.host, "@"); k >= 0 {
			t.user, t.host = t.host[:k], t.host[k+1:]
		}
		t.host = strings.Trim(t.host, "[]")
	}
	if t.host == "" || strings.HasPrefix(t.host, "-") || strings.HasPrefix(t.user, "-") {
		return sshTarget{}, false
	}
	return t, true
}

// sshTargets returns the distinct ssh destinations of repos that use the
// default ssh command.
func sshTargets(repos []Repo, info map[string]repoInfo) []sshTarget {
	seen := map[sshTarget]bool{}
	var ts []sshTarget
	for _, r := range repos {
		in := info[r.Dir]
		if in.ownSSH {
			continue
		}
		if t, ok := parseSSHRemote(in.remote); ok && !seen[t] {
			seen[t] = true
			ts = append(ts, t)
		}
	}
	return ts
}

// sshMux owns a per-run ControlPath directory and the masters opened in it.
type sshMux struct {
	dir     string
	opts    []string
	targets []sshTarget
}

func newSSHMux() (*sshMux, error) {
	base := os.TempDir()
	if len(base) > 40 { // unix socket paths are limited to 104 bytes on macOS
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "clr")
	if err != nil {
		return nil, err
	}
	return &sshMux{dir: dir, opts: []string{
		"-o", "BatchMode=yes", "-o", "ControlMaster=auto",
		"-o", "ControlPath=" + dir + "/%C", "-o", "ControlPersist=60",
	}}, nil
}

func (m *sshMux) command() string { return "ssh " + strings.Join(m.opts, " ") }

// prewarm opens a master for t. Failure is not fatal: jobs for that host
// just connect on their own.
func (m *sshMux) prewarm(ctx context.Context, t sshTarget) {
	if ctx.Err() != nil {
		return
	}
	m.targets = append(m.targets, t)
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "ssh", append(append(slices.Clone(m.opts), "-MNf"), t.args()...)...)
	cmd.WaitDelay = time.Second // -f forks; don't wait on inherited fds
	_ = cmd.Run()
}

// close stops every master (jobs may also have started one) and removes dir.
func (m *sshMux) close() {
	for _, t := range m.targets {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "ssh", append([]string{"-o", "ControlPath=" + m.dir + "/%C", "-O", "exit"}, t.args()...)...)
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
		cancel()
	}
	os.RemoveAll(m.dir)
}

func syncRepo(ctx context.Context, r Repo, o SyncOpts, env []string) RepoResult {
	res := RepoResult{Repo: r}
	if r.Bad != "" {
		res.Status, res.Err = StFailed, r.Bad
		return res
	}
	if err := ctx.Err(); err != nil {
		res.Status, res.Err = StFailed, "cancelled"
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	fail := func(stderr string, err error) RepoResult {
		res.Status = StFailed
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			res.Err = "timed out after " + o.Timeout.String()
		case ctx.Err() != nil:
			res.Err = "cancelled"
		case errLine(stderr) != "":
			res.Err = errLine(stderr)
		default:
			res.Err = err.Error()
		}
		return res
	}

	if !fileExists(r.Dir) {
		if !o.Clone {
			res.Status = StMissing
			return res
		}
		switch {
		case r.URL == "":
			res.Status, res.Err = StFailed, "missing and no clone URL"
			return res
		case strings.HasPrefix(r.URL, "-"):
			res.Status, res.Err = StFailed, fmt.Sprintf("invalid clone URL %q", r.URL)
			return res
		}
		created := firstMissing(filepath.Dir(r.Dir))
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return fail("", err)
		}
		stdout, stderr, err := git(ctx, env, "", "clone", "--", r.URL, r.Dir)
		res.Output = stdout + stderr
		if err != nil {
			os.RemoveAll(r.Dir) // did not exist before this run
			if created != "" {
				os.RemoveAll(created)
			}
			return fail(stderr, err)
		}
		res.Status = StCloned
		return res
	}
	if !isRepo(r.Dir) {
		res.Status, res.Err = StFailed, "not a git repository"
		return res
	}

	if st, _, err := git(ctx, env, r.Dir, "status", "--porcelain"); err == nil {
		res.Dirty = strings.TrimSpace(st) != ""
	}
	before, _, _ := git(ctx, env, r.Dir, "rev-parse", "HEAD")
	stdout, stderr, err := git(ctx, env, r.Dir, "pull", "--ff-only")
	res.Output = stdout + stderr
	if err != nil {
		return fail(stderr, err)
	}
	after, _, _ := git(ctx, env, r.Dir, "rev-parse", "HEAD")
	if before != after {
		res.Status = StUpdated
	} else {
		res.Status = StUpToDate
	}
	return res
}

// firstMissing returns the topmost ancestor of p (or p) that doesn't exist.
func firstMissing(p string) string {
	missing := ""
	for !fileExists(p) {
		missing = p
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return missing
}

// git runs git in its own process group. Cancelling ctx sends SIGTERM to the
// group (git and its ssh); WaitDelay is the SIGKILL fallback. A nil env
// means the plain non-prompting environment.
func git(ctx context.Context, env []string, dir string, args ...string) (string, string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	if env == nil {
		env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// errLine picks the first meaningful stderr line, skipping fetch progress
// ("From …", ref updates, "remote: …") and hints; else the first non-empty line.
func errLine(s string) string {
	first := ""
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if first == "" {
			first = t
		}
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "From ") ||
			strings.HasPrefix(l, "remote:") || strings.HasPrefix(l, "hint:") {
			continue
		}
		return t
	}
	return first
}

// summarize returns "N updated · N up to date · N failed · N dirty · N missing".
func summarize(rs []RepoResult) string {
	var up, same, failed, dirty, missing, cloned int
	for _, r := range rs {
		switch r.Status {
		case StUpdated:
			up++
		case StUpToDate:
			same++
		case StFailed:
			failed++
		case StMissing:
			missing++
		case StCloned:
			cloned++
		}
		if r.Dirty {
			dirty++
		}
	}
	s := fmt.Sprintf("%d updated · %d up to date · %d failed · %d dirty · %d missing", up, same, failed, dirty, missing)
	if cloned > 0 {
		s += fmt.Sprintf(" · %d cloned", cloned)
	}
	return s
}

func statusIcon(s RepoStatus) string {
	switch s {
	case StUpdated, StCloned:
		return "✓"
	case StUpToDate:
		return "="
	case StFailed:
		return "✗"
	case StMissing:
		return "∅"
	}
	return "·"
}

// sortResults puts failures first, keeping path order otherwise.
func sortResults(rs []RepoResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		return rs[i].Status == StFailed && rs[j].Status != StFailed
	})
}
