//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rels(rs []Repo) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Rel)
	}
	return out
}

func TestDiscoverMeta(t *testing.T) {
	d := t.TempDir()
	writeFile(t, d+"/.meta", `{"projects":{"a":"u-a","libs":"u-libs","g/nested":"u-n"}}`)
	writeFile(t, d+"/libs/.meta", `{"projects":{"x":"u-x"}}`)
	writeFile(t, d+"/a/inner/.meta", `{"projects":{"zzz":"u-z"}}`) // inside a project without its own .meta
	writeFile(t, d+"/plain/deep/.meta", `{"projects":{"p":"u-p"}}`)
	writeFile(t, d+"/.hid/.meta", `{"projects":{"h":"u-h"}}`)
	mkdirs(t, d, "a/inner")

	repos, err := discover(d, defaultIgnore, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "g/nested", "libs", "libs/x", "plain/deep/p"}; !reflect.DeepEqual(rels(repos), want) {
		t.Fatalf("repos = %v, want %v", rels(repos), want)
	}
	if repos[3].URL != "u-x" || repos[3].Dir != filepath.Join(d, "libs/x") {
		t.Fatalf("libs/x = %+v", repos[3])
	}
	repos, _ = discover(d, defaultIgnore, []string{"plain", "libs/**"})
	if want := []string{"a", "g/nested"}; !reflect.DeepEqual(rels(repos), want) {
		t.Fatalf("with exclude = %v, want %v", rels(repos), want)
	}
}

func TestDiscoverFallback(t *testing.T) {
	d := t.TempDir()
	mkdirs(t, d, "r1/.git", "group/r2/.git", "group/sub/r3/.git", ".hidden/r4/.git", "r1/nested/.git", "plain")
	writeFile(t, d+"/group/wt/.git", "gitdir: elsewhere") // worktree-style .git file
	repos, err := discover(d, defaultIgnore, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"group/r2", "group/sub/r3", "group/wt", "r1"}; !reflect.DeepEqual(rels(repos), want) {
		t.Fatalf("fallback = %v, want %v", rels(repos), want)
	}
	repos, _ = discover(d+"/r1", defaultIgnore, nil)
	if want := []string{".", "nested"}; !reflect.DeepEqual(rels(repos), want) { // the target is always descended
		t.Fatalf("target repo itself = %v", rels(repos))
	}
}

func TestErrLine(t *testing.T) {
	in := "From git.example:x/y\n * branch main -> FETCH_HEAD\nremote: Counting\nhint: diverged\nfatal: Not possible to fast-forward, aborting.\n"
	if got := errLine(in); got != "fatal: Not possible to fast-forward, aborting." {
		t.Errorf("errLine = %q", got)
	}
	if got := errLine("sign_and_send_pubkey: failed\nfatal: Could not read\n"); got != "sign_and_send_pubkey: failed" {
		t.Errorf("errLine = %q", got)
	}
	if got := errLine("remote: denied\n"); got != "remote: denied" {
		t.Errorf("fallback = %q", got)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"archive", "archive", true},
		{"archive/**", "archive/x/y", true},
		{"**/old", "a/b/old", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/x/c", false},
		{"arch", "archive", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pat, c.name, got)
		}
	}
	if !excluded("archive/x", []string{"archive"}) || excluded("other", []string{"archive"}) {
		t.Error("excluded wrong")
	}
}

// gitT runs git for test setup with an isolated config.
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func isolateGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
}

// remoteFixture creates a bare repo with one commit and returns it with a
// working clone ("seed") used to push further commits.
func remoteFixture(t *testing.T, d string) (bare, seed string) {
	t.Helper()
	bare, seed = filepath.Join(d, "remote.git"), filepath.Join(d, "seed")
	gitT(t, d, "init", "-q", "--bare", "-b", "main", bare)
	gitT(t, d, "clone", "-q", bare, seed)
	writeFile(t, seed+"/f", "1")
	gitT(t, seed, "add", "f")
	gitT(t, seed, "commit", "-q", "-m", "one")
	gitT(t, seed, "push", "-q", "origin", "main")
	return bare, seed
}

func pushCommit(t *testing.T, seed, content string) {
	t.Helper()
	writeFile(t, seed+"/f", content)
	gitT(t, seed, "commit", "-q", "-am", content)
	gitT(t, seed, "push", "-q", "origin", "main")
}

func TestSyncClassification(t *testing.T) {
	isolateGit(t)
	d := t.TempDir()
	bare, seed := remoteFixture(t, d)
	ws := filepath.Join(d, "ws")
	for _, name := range []string{"behind", "diverged", "dirty"} {
		gitT(t, d, "clone", "-q", bare, filepath.Join(ws, name))
	}
	pushCommit(t, seed, "2")
	gitT(t, d, "clone", "-q", bare, filepath.Join(ws, "uptodate"))
	writeFile(t, ws+"/diverged/g", "local")
	gitT(t, ws+"/diverged", "add", "g")
	gitT(t, ws+"/diverged", "commit", "-q", "-m", "local")
	pushCommit(t, seed, "3")
	gitT(t, ws+"/diverged", "fetch", "-q") // so pull cannot fast-forward
	writeFile(t, ws+"/dirty/untracked", "x")
	writeFile(t, ws+"/.meta", `{"projects":{"behind":"`+bare+`","diverged":"`+bare+`","dirty":"`+bare+`","uptodate":"`+bare+`","missing":"`+bare+`"}}`)
	// make uptodate current with commit 3
	gitT(t, ws+"/uptodate", "pull", "-q", "--ff-only")

	repos, err := discover(ws, defaultIgnore, nil)
	if err != nil || len(repos) != 5 {
		t.Fatalf("discover: %v %v", rels(repos), err)
	}
	run := func(rs []Repo, clone bool) map[string]RepoResult {
		got := map[string]RepoResult{}
		running := 0
		for ev := range runSync(context.Background(), rs, SyncOpts{Jobs: 3, Timeout: 30 * time.Second, Clone: clone}) {
			if ev.Result.Status == StRunning {
				running++
				continue
			}
			got[ev.Result.Rel] = ev.Result
		}
		if running != len(rs) { // no transient failures, so no retries
			t.Errorf("running events = %d, want %d", running, len(rs))
		}
		return got
	}
	got := run(repos, false)
	want := map[string]RepoStatus{"behind": StUpdated, "diverged": StFailed, "dirty": StUpdated, "uptodate": StUpToDate, "missing": StMissing}
	for rel, st := range want {
		if got[rel].Status != st {
			t.Errorf("%s: status %v, want %v (err %q)", rel, got[rel].Status, st, got[rel].Err)
		}
	}
	if !got["dirty"].Dirty || got["behind"].Dirty {
		t.Error("dirty flag wrong")
	}
	if !strings.HasPrefix(got["diverged"].Err, "fatal:") || !strings.Contains(got["diverged"].Output, "fast-forward") {
		t.Errorf("diverged should keep stderr: %q / %q", got["diverged"].Err, got["diverged"].Output)
	}
	var all []RepoResult
	for _, r := range got {
		all = append(all, r)
	}
	if s := summarize(all); s != "2 updated · 1 up to date · 1 failed · 1 dirty · 1 missing" {
		t.Errorf("summary = %q", s)
	}
	sortResults(all)
	if all[0].Status != StFailed {
		t.Error("failures must sort first")
	}

	var missing []Repo
	for _, r := range repos {
		if r.Rel == "missing" {
			missing = append(missing, r)
		}
	}
	if r := run(missing, true)["missing"]; r.Status != StCloned || !isRepo(filepath.Join(ws, "missing")) {
		t.Fatalf("clone-missing: %+v", r)
	}
}

func TestParseSSHRemote(t *testing.T) {
	cases := []struct {
		url  string
		want sshTarget
		ok   bool
	}{
		{"git@git.example.com:group/x.git", sshTarget{user: "git", host: "git.example.com"}, true},
		{"github.com:me/x.git", sshTarget{host: "github.com"}, true}, // user: ssh default
		{"ssh://git@host.example:2222/x.git", sshTarget{user: "git", host: "host.example", port: "2222"}, true},
		{"ssh://host.example/x.git", sshTarget{host: "host.example"}, true},
		{"git+ssh://u@h/x", sshTarget{user: "u", host: "h"}, true},
		{"https://github.com/me/x.git", sshTarget{}, false},
		{"git://host/x", sshTarget{}, false},
		{"file:///srv/x.git", sshTarget{}, false},
		{"/srv/remote.git", sshTarget{}, false},
		{"./a:b", sshTarget{}, false},
		{"", sshTarget{}, false},
	}
	for _, c := range cases {
		got, ok := parseSSHRemote(c.url)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSSHRemote(%q) = %+v, %v; want %+v, %v", c.url, got, ok, c.want, c.ok)
		}
	}
	if got := (sshTarget{user: "git", host: "h", port: "22"}).args(); !reflect.DeepEqual(got, []string{"-p", "22", "--", "git@h"}) {
		t.Errorf("args = %v", got)
	}
}

func TestAuthTransient(t *testing.T) {
	agent := `sign_and_send_pubkey: signing failed for ED25519 "k" from agent: communication with agent failed`
	cases := []struct {
		r    RepoResult
		want bool
	}{
		{RepoResult{Status: StFailed, Err: agent, Output: agent + "\nfatal: Could not read from remote repository."}, true},
		{RepoResult{Status: StFailed, Output: "foo: agent refused operation\n"}, true},
		{RepoResult{Status: StFailed, Err: "fatal: Not possible to fast-forward, aborting."}, false},
		{RepoResult{Status: StFailed, Err: "Permission denied (publickey)."}, false},
		{RepoResult{Status: StUpToDate, Output: agent}, false},
	}
	for i, c := range cases {
		if got := authTransient(c.r); got != c.want {
			t.Errorf("case %d: authTransient = %v, want %v", i, got, c.want)
		}
	}
}

func TestSSHMuxCleanup(t *testing.T) {
	m, err := newSSHMux()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.dir+"/0123456789012345678901234567890123456789") > 104 {
		t.Fatalf("ControlPath too long: %s", m.dir)
	}
	m.close()
	if fileExists(m.dir) {
		t.Fatalf("%s not removed", m.dir)
	}
}

func badRows(rs []Repo) map[string]string {
	m := map[string]string{}
	for _, r := range rs {
		if r.Bad != "" {
			m[r.Rel] = r.Bad
		}
	}
	return m
}

func TestDiscoverMetaTraversal(t *testing.T) {
	d := t.TempDir()
	outside := t.TempDir()
	writeFile(t, d+"/.meta", `{"projects":{"../x":"u","a/../../x":"u","/abs":"u","link/p":"u",".":"u","ok":"u"}}`)
	if err := os.Symlink(outside, d+"/link"); err != nil {
		t.Fatal(err)
	}
	repos, err := discover(d, defaultIgnore, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := badRows(repos)
	for _, k := range []string{"../x", "a/../../x", "/abs", "link/p", "."} {
		if bad[".meta: "+k] == "" {
			t.Errorf("key %q not rejected; bad rows: %v", k, bad)
		}
	}
	if len(bad) != 5 {
		t.Errorf("bad rows = %v", bad)
	}
	var ok bool
	for _, r := range repos {
		ok = ok || (r.Rel == "ok" && r.Bad == "")
	}
	if !ok {
		t.Error("valid key missing")
	}
	// a bad row is reported as failed without touching git
	if r := syncRepo(context.Background(), repos[0], SyncOpts{Timeout: time.Second}, nil); repos[0].Bad != "" && r.Status != StFailed {
		t.Errorf("bad row status = %v", r.Status)
	}
}

func TestDiscoverMetaParseError(t *testing.T) {
	d := t.TempDir()
	writeFile(t, d+"/g/.meta", `{"projects":`)
	repos, _ := discover(d, defaultIgnore, nil)
	if bad := badRows(repos); !strings.HasPrefix(bad["g/.meta"], "cannot read .meta") {
		t.Fatalf("parse error not reported: %+v", repos)
	}
}

// A meta root with a nested clone of itself: the target is a repo without a
// root .meta, group dirs with .meta files, a second clone of the meta repo
// listed as a project (hub/selfclone, whose nested .meta files must not be
// read), .meta directories that are not meta files, and plain repos.
func TestDiscoverMetaRootWithNestedSelfClone(t *testing.T) {
	d := t.TempDir()
	mkdirs(t, d, ".git", "group/sub/r1/.git", "hub/selfclone/.git", "hub/selfclone/misc/x/.git",
		"configs/etc/sv/ssh/.meta", "plain/repoA/.git", "plain/repoA/sub/.git", "metarepo/.git")
	writeFile(t, d+"/group/sub/.meta", `{"projects":{"r1":"u-r1"}}`)
	writeFile(t, d+"/hub/.meta", `{"projects":{"selfclone":"u-m","tool":"u-t"}}`)
	writeFile(t, d+"/hub/selfclone/misc/.meta", `{"projects":{"x":"u-x"}}`)
	writeFile(t, d+"/metarepo/.meta", `{"projects":{"child":"u-c"}}`)
	repos, err := discover(d, defaultIgnore, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", "group/sub/r1", "hub/selfclone", "hub/tool", "metarepo", "metarepo/child", "plain/repoA"}
	if !reflect.DeepEqual(rels(repos), want) {
		t.Fatalf("repos = %v\nwant    %v", rels(repos), want)
	}
	meta := map[string]bool{}
	for _, r := range repos {
		meta[r.Rel] = r.Meta
	}
	if !meta["."] || !meta["metarepo"] || meta["hub/selfclone"] || meta["plain/repoA"] {
		t.Fatalf("meta flags = %v", meta)
	}
	if len(badRows(repos)) != 0 {
		t.Fatalf("unexpected bad rows: %v", badRows(repos))
	}
}

func TestCloneRejectsDashURL(t *testing.T) {
	d := t.TempDir()
	r := syncRepo(context.Background(), Repo{Dir: d + "/x", URL: "--upload-pack=touch " + d + "/pwned"},
		SyncOpts{Clone: true, Timeout: 5 * time.Second}, nil)
	if r.Status != StFailed || !strings.Contains(r.Err, "invalid clone URL") || fileExists(d+"/pwned") || fileExists(d+"/x") {
		t.Fatalf("result %+v", r)
	}
}

// The meta repo is pulled first; a project added to its .meta upstream then
// shows up as missing in the same run.
func TestSyncMetaRepoFirst(t *testing.T) {
	isolateGit(t)
	d := t.TempDir()
	mkdirs(t, d, "m", "p1")
	metaBare, metaSeed := remoteFixture(t, d+"/m")
	p1Bare, _ := remoteFixture(t, d+"/p1")
	writeFile(t, metaSeed+"/.meta", `{"projects":{"p1":"`+p1Bare+`"}}`)
	gitT(t, metaSeed, "add", ".meta")
	gitT(t, metaSeed, "commit", "-q", "-m", "meta")
	gitT(t, metaSeed, "push", "-q", "origin", "main")
	group := filepath.Join(d, "ws/group")
	gitT(t, d, "clone", "-q", metaBare, group)
	gitT(t, d, "clone", "-q", p1Bare, group+"/p1")
	writeFile(t, metaSeed+"/.meta", `{"projects":{"p1":"`+p1Bare+`","p2":"`+p1Bare+`"}}`)
	gitT(t, metaSeed, "commit", "-q", "-am", "add p2")
	gitT(t, metaSeed, "push", "-q", "origin", "main")

	repos, err := discover(group, defaultIgnore, nil)
	if err != nil || !reflect.DeepEqual(rels(repos), []string{".", "p1"}) || !repos[0].Meta {
		t.Fatalf("discover: %v %+v", err, repos)
	}
	o := SyncOpts{Jobs: 4, Timeout: 30 * time.Second,
		Rediscover: func() ([]Repo, error) { return discover(group, defaultIgnore, nil) }}
	list := repos
	final := map[string]RepoStatus{}
	var order []string
	for ev := range runSync(context.Background(), repos, o) {
		switch {
		case ev.Repos != nil:
			list = ev.Repos
		case ev.Note == "" && ev.Result.Status > StRunning:
			final[list[ev.Index].Rel] = ev.Result.Status
			order = append(order, list[ev.Index].Rel)
		}
	}
	if !reflect.DeepEqual(rels(list), []string{".", "p1", "p2"}) {
		t.Fatalf("list after rediscover = %v", rels(list))
	}
	want := map[string]RepoStatus{".": StUpdated, "p1": StUpToDate, "p2": StMissing}
	if !reflect.DeepEqual(final, want) {
		t.Fatalf("statuses = %v, want %v", final, want)
	}
	if order[0] != "." {
		t.Fatalf("meta repo not first: %v", order)
	}
}

func TestSyncCancelKillsClone(t *testing.T) {
	d := t.TempDir()
	fake := filepath.Join(d, "fakessh-clauder-test")
	writeFile(t, fake, "#!/bin/sh\nsleep 30\n")
	os.Chmod(fake, 0o755)
	t.Setenv("GIT_SSH_COMMAND", fake)
	dest := filepath.Join(d, "ws/sub/repo")
	ctx, cancel := context.WithCancel(context.Background())
	ch := runSync(ctx, []Repo{{Dir: dest, Rel: "sub/repo", URL: "fakehost:repo.git"}}, SyncOpts{Clone: true, Timeout: time.Minute})
	time.AfterFunc(700*time.Millisecond, cancel)
	closed := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
	if fileExists(filepath.Join(d, "ws")) {
		t.Error("half-created clone dirs not removed")
	}
	time.Sleep(200 * time.Millisecond)
	if out, err := exec.Command("pgrep", "-f", fake).Output(); err == nil {
		t.Errorf("leftover processes: %s", out)
	}
}
