//go:build unix

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	"unicode"
	"unicode/utf8"
)

// Repo is one git repository to sync.
type Repo struct {
	Dir  string // absolute
	Rel  string // relative to the sync target
	URL  string // clone URL from a .meta file, may be empty
	Meta bool   // repo holds .meta files: pulled before the others
	Bad  string // invalid .meta entry, reported as a failed row
	Base string // the .meta directory a listed project must stay inside
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
			r.Base = cmp.Or(r.Base, old.Base)
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
		bad := func(key, reason string) {
			add(Repo{Dir: file + "\x00" + key, Rel: relTo(file) + ": " + key, Bad: reason})
		}
		for key, u := range m.Projects {
			p, err := metaProjectPath(dir, key)
			if err != nil {
				bad(key, err.Error())
				continue
			}
			add(Repo{Dir: p, URL: u, Base: dir})
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

	refuseNested(projects, relTo)

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

// refuseNested turns a listed project into a failed row when another repo
// or project (from any .meta file, or a plain repo) sits between its .meta
// directory and itself: a checkout of that outer repo could plant a symlink
// that redirects the inner clone. Paths compare case-insensitively (APFS).
// A project inside its own meta repo, or inside repos above the .meta
// directory, is the normal meta layout and stays allowed.
func refuseNested(projects map[string]Repo, relTo func(string) string) {
	repoAt := map[string]string{} // lowercased path -> path
	for dir, r := range projects {
		if r.Bad == "" {
			repoAt[strings.ToLower(dir)] = dir
		}
	}
	for dir, r := range projects {
		if r.Bad != "" || r.Base == "" {
			continue
		}
		base := strings.ToLower(r.Base)
		for a := filepath.Dir(dir); inside(r.Base, a, false); a = filepath.Dir(a) {
			outer, ok := repoAt[strings.ToLower(a)]
			if !ok || strings.ToLower(a) == base {
				continue
			}
			r.Bad = fmt.Sprintf("project path %q is nested inside project %q", relTo(dir), relTo(outer))
			projects[dir] = r
			break
		}
		// a sibling that differs only in case is the same directory on APFS
		if other, ok := repoAt[strings.ToLower(dir)]; ok && other != dir && r.Bad == "" {
			r.Bad = fmt.Sprintf("project path %q collides with %q (case)", relTo(dir), relTo(other))
			projects[dir] = r
		}
	}
}

// unsafeRune reports control and invisible formatting characters (ESC, CR,
// NUL, bidi overrides, …) that could rewrite the terminal or disguise a host.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// validKeyPart reports whether one path component of a .meta key uses only
// printable ASCII letters, digits and ._-+@ and does not start with a dot.
// This rules out .git (in any case), traversal, control and bidi characters,
// Unicode normalisation and case tricks.
func validKeyPart(c string) bool {
	if c == "" || c[0] == '.' {
		return false
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case b == '.', b == '_', b == '-', b == '+', b == '@':
		default:
			return false
		}
	}
	return true
}

// metaProjectPath resolves a .meta project key to a directory strictly inside
// dir, also after resolving symlinks of its nearest existing parent.
func metaProjectPath(dir, key string) (string, error) {
	for _, c := range strings.Split(key, "/") {
		if !validKeyPart(c) {
			return "", fmt.Errorf("unsupported character in project path: %q", key)
		}
	}
	p := filepath.Join(dir, filepath.FromSlash(key))
	if !inside(dir, p, false) {
		return "", fmt.Errorf("project path %q points outside the .meta directory", key)
	}
	if err := checkContained(dir, p); err != nil {
		return "", fmt.Errorf("project path %q: %v", key, err)
	}
	return p, nil
}

// checkContained verifies that p, after resolving the symlinks of its nearest
// existing ancestor (or p itself), still lies inside base.
func checkContained(base, p string) error {
	if !inside(base, p, true) {
		return errors.New("points outside the .meta directory")
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	a := p
	for a != base && !fileExists(a) {
		a = filepath.Dir(a)
	}
	realA, err := filepath.EvalSymlinks(a)
	if err != nil {
		return err
	}
	if !inside(realBase, realA, true) {
		return errors.New("resolves outside the .meta directory via a symlink")
	}
	return nil
}

// removeIn deletes p below base through an os.Root, so a symlink swapped in
// meanwhile cannot redirect the deletion. It refuses paths that escape base,
// also via symlinks. Non-recursive removal leaves non-empty dirs alone.
func removeIn(base, p string, recursive bool) error {
	if !inside(base, p, false) {
		return errors.New("refusing to remove a path outside the .meta directory")
	}
	if err := checkContained(base, filepath.Dir(p)); err != nil {
		return err
	}
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	if recursive {
		return root.RemoveAll(rel)
	}
	return root.Remove(rel)
}

// mkdirAllIn creates dir below base one level at a time through an os.Root
// (symlinks cannot lead outside base) and returns the dirs it created,
// outermost first.
func mkdirAllIn(base, dir string) ([]string, error) {
	rel, err := filepath.Rel(base, dir)
	if err != nil || !inside(base, dir, true) {
		return nil, errors.New("refusing to create a directory outside the .meta directory")
	}
	if rel == "." {
		return nil, nil
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var created []string
	cur := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		switch err := root.Mkdir(cur, 0o755); {
		case err == nil:
			created = append(created, filepath.Join(base, cur))
		case errors.Is(err, fs.ErrExist):
		default:
			return created, err
		}
	}
	return created, nil
}

// cleanupClone removes a failed clone: the target dir (which did not exist
// before) recursively, then the parents this job created, deepest first and
// only while empty.
func cleanupClone(base, target string, parents []string) {
	removeIn(base, target, true)
	for i := len(parents) - 1; i >= 0; i-- {
		removeIn(base, parents[i], false)
	}
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
	// HostsFrom are existing repos whose origin hosts may be prewarmed even
	// though they are not synced in this run (the TUI's clone-missing step).
	HostsFrom []Repo
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
	origin string // origin URL of an existing repo ("" for missing ones)
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
				trusted := trustedHosts(info)
				if len(o.HostsFrom) > 0 {
					for t := range trustedHosts(inspectRepos(ctx, o.HostsFrom, o.Jobs)) {
						trusted[t] = true
					}
				}
				for _, t := range prewarmTargets(repos, info, trusted, o.Clone) {
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
		// pulls first; clones only after every pull and checkout has finished,
		// so no concurrent checkout can plant a symlink under a clone target
		var pulls, clones []int
		for i, r := range cur {
			switch {
			case done[i]:
			case r.Bad == "" && !fileExists(r.Dir):
				clones = append(clones, i)
			default:
				pulls = append(pulls, i)
			}
		}
		pool(pulls)
		pool(clones)

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
						in.origin = strings.TrimSpace(out)
					} else {
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
	if t.host == "" || strings.HasPrefix(t.host, "-") || strings.HasPrefix(t.user, "-") ||
		strings.ContainsFunc(t.host+t.user, unsafeRune) {
		return sshTarget{}, false
	}
	return t, true
}

// trustedHosts returns the ssh destinations that existing repos' origins
// already use. Only these are ever prewarmed.
func trustedHosts(info map[string]repoInfo) map[sshTarget]bool {
	trusted := map[sshTarget]bool{}
	for _, in := range info {
		if t, ok := parseSSHRemote(in.origin); ok && !in.ownSSH {
			trusted[t] = true
		}
	}
	return trusted
}

// prewarmTargets returns the distinct ssh destinations to open masters for:
// origins of existing repos, plus (only when cloning) .meta URLs of missing
// repos whose host is already trusted. A plain pull never connects anywhere
// for a missing repo, and a host known only from a .meta file gets no master.
func prewarmTargets(repos []Repo, info map[string]repoInfo, trusted map[sshTarget]bool, clone bool) []sshTarget {
	seen := map[sshTarget]bool{}
	var ts []sshTarget
	for _, r := range repos {
		in := info[r.Dir]
		raw := in.origin
		if raw == "" && clone && r.Bad == "" && !fileExists(r.Dir) {
			raw = r.URL
		}
		if in.ownSSH || raw == "" {
			continue
		}
		if t, ok := parseSSHRemote(raw); ok && trusted[t] && !seen[t] {
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

	if r.Base != "" { // a listed project: its path must still resolve inside the .meta dir
		if err := checkContained(r.Base, r.Dir); err != nil {
			res.Status, res.Err = StFailed, "refused: "+err.Error()
			return res
		}
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
		if r.Base == "" {
			res.Status, res.Err = StFailed, "refused: not listed in a .meta file"
			return res
		}
		parents, err := mkdirAllIn(r.Base, filepath.Dir(r.Dir))
		if err != nil {
			cleanupClone(r.Base, r.Dir, parents)
			return fail("", err)
		}
		if err := checkContained(r.Base, r.Dir); err != nil || fileExists(r.Dir) { // re-check right before cloning
			for i := len(parents) - 1; i >= 0; i-- {
				removeIn(r.Base, parents[i], false)
			}
			res.Status, res.Err = StFailed, "refused: changed on disk before cloning"
			if err != nil {
				res.Err = "refused: " + err.Error()
			}
			return res
		}
		stdout, stderr, err := git(ctx, env, "", "clone", "--", r.URL, r.Dir)
		res.Output = stdout + stderr
		if err != nil {
			cleanupClone(r.Base, r.Dir, parents) // only what this job created, only inside Base
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

// sanitize makes untrusted text (paths from .meta files, git output) safe to
// print: control and formatting characters become \uXXXX, except newlines
// and tabs when multiline is set.
func sanitize(s string, multiline bool) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 { // invalid byte, e.g. a raw C1 CSI 0x9B
			fmt.Fprintf(&b, "\\x%02X", s[i])
			i++
			continue
		}
		i += size
		switch {
		case multiline && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "\\u%04X", r)
		}
	}
	return b.String()
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
