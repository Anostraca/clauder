//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Session is one Claude Code conversation recorded for a folder.
type Session struct {
	ID, Dir, Title, Account string // Dir: folder to run in; Account: "" without accounts
	When                    time.Time
}

const (
	maxSessionFiles = 300       // newest session files opened per scan
	sessionChunk    = 256 << 10 // bytes read from each end of a session file
	maxKeyLen       = 200       // Claude hashes project dir names past this
	maxTitleLen     = 200       // runes kept of a title
)

// projectKey is Claude Code's project dir name for startup folder p: every
// UTF-16 unit outside [a-zA-Z0-9] becomes "-". Past maxKeyLen Claude keeps
// the first maxKeyLen chars and adds "-<hash>"; keyMatches allows for that.
func projectKey(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF: // a surrogate pair in UTF-16
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// baseKey is projectKey for a scope base; "" for "/", so that "-" + a
// child's key extends it like any other.
func baseKey(p string) string {
	if p == "/" {
		return ""
	}
	return projectKey(p)
}

// plainID reports whether a session file name is safe as an argv value:
// [A-Za-z0-9] first, then only [A-Za-z0-9_-].
func plainID(id string) bool {
	for i, r := range id {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || r != '_' && r != '-') {
			return false
		}
	}
	return id != ""
}

// keyMatches reports whether project dir name may hold sessions started in
// the folder encoded as key or below it. The cwd check decides.
func keyMatches(name, key string) bool {
	if len(key) > maxKeyLen {
		return strings.HasPrefix(name, key[:maxKeyLen])
	}
	return name == key || strings.HasPrefix(name, key+"-")
}

// sessionSrc is a Claude config dir to scan and the accounts using it.
type sessionSrc struct {
	dir   string
	accts []string // empty when no accounts are configured
}

// sessionSources lists the config dirs of all accounts, deduplicated by real
// path; without accounts $CLAUDE_CONFIG_DIR, else ~/.claude.
func sessionSources(c *Config) []sessionSrc {
	if len(c.Accounts) == 0 {
		dir := defaultClaudeDir()
		if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
			dir = expandPath(env)
		}
		return []sessionSrc{{dir: dir}}
	}
	var out []sessionSrc
	seen := map[string]int{}
	for _, a := range c.Accounts {
		dir := defaultClaudeDir()
		if !isDefaultClaudeDir(a.ConfigDir) {
			dir = expandPath(a.ConfigDir)
		}
		if i, ok := seen[realPath(dir)]; ok {
			out[i].accts = append(out[i].accts, a.Name)
			continue
		}
		seen[realPath(dir)] = len(out)
		out = append(out, sessionSrc{dir: dir, accts: []string{a.Name}})
	}
	return out
}

// owner is the account a session in dir found in s belongs to: the one
// resolved for dir when it shares s, else the first.
func (s sessionSrc) owner(c *Config, dir string) string {
	if len(s.accts) == 0 {
		return ""
	}
	if a, _ := c.ResolveAccount(dir); a != nil && slices.Contains(s.accts, a.Name) {
		return a.Name
	}
	return s.accts[0]
}

// keyIs reports whether project dir name is the one for key.
func keyIs(name, key string) bool {
	if len(key) > maxKeyLen {
		return strings.HasPrefix(name, key[:maxKeyLen])
	}
	return name == key
}

// projectResolver returns resolveProject cached per project dir name and
// safe for concurrent use; the hint of the first caller decides.
func projectResolver(scope []scopeBase) func(name, hint string) (string, bool) {
	type res struct {
		once sync.Once
		dir  string
		ok   bool
	}
	var mu sync.Mutex
	cache := map[string]*res{}
	return func(name, hint string) (string, bool) {
		mu.Lock()
		r := cache[name]
		if r == nil {
			r = &res{}
			cache[name] = r
		}
		mu.Unlock()
		r.once.Do(func() { r.dir, r.ok = resolveProject(name, hint, scope) })
		return r.dir, r.ok
	}
}

// maxProjectHits bounds the folders one ambiguous project dir name is
// resolved to ("a-b" is also "a/b").
const maxProjectHits = 8

// resolveProject finds the existing folder under scope whose project dir is
// name, walking down only where the encoded path still matches. Of several
// hits it prefers one named hint (the recorded cwd's base name), else the
// first. A hit through a real path is mapped back under the base as given.
// Hashed names are not resolved.
func resolveProject(name, hint string, scope []scopeBase) (string, bool) {
	if len(name) > maxKeyLen {
		return "", false
	}
	var hits []string
	var walk func(dir, key string, add func(string))
	walk = func(dir, key string, add func(string)) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if len(hits) >= maxProjectHits {
				return
			}
			if !e.IsDir() {
				continue
			}
			child, ck := filepath.Join(dir, e.Name()), key+"-"+projectKey(e.Name())
			if name == ck {
				add(child)
			} else if strings.HasPrefix(name, ck+"-") {
				walk(child, ck, add)
			}
		}
	}
	for _, b := range scope {
		for _, base := range []string{b.given, b.real} {
			add := func(p string) { hits = append(hits, filepath.Join(b.given, strings.TrimPrefix(p, base))) }
			key := baseKey(base)
			if name == key {
				add(base)
			} else if strings.HasPrefix(name, key+"-") {
				walk(base, key, add)
			}
		}
	}
	if len(hits) == 0 {
		return "", false
	}
	for _, h := range hits {
		if filepath.Base(h) == hint {
			return h, true
		}
	}
	return hits[0], true
}

// scopeBase is a base folder as given and with symlinks resolved.
type scopeBase struct{ given, real string }

// inScope maps a recorded cwd to the folder to show and run in: under a base
// as given, or moved back under it when cwd went through the real path.
func inScope(cwd string, bases []scopeBase) (string, bool) {
	for _, b := range bases {
		if cwd == b.given || within(cwd, b.given) {
			return cwd, true
		}
	}
	for _, b := range bases {
		if b.real != b.given && (cwd == b.real || within(cwd, b.real)) {
			return filepath.Join(b.given, strings.TrimPrefix(cwd, b.real)), true
		}
	}
	return "", false
}

// findSessions lists the sessions recorded in bases and their subfolders,
// newest first. Only the newest maxSessionFiles files are read; cut reports
// that more exist.
func findSessions(c *Config, bases []string) (list []Session, cut bool) {
	var scope []scopeBase
	var keys []string
	for _, b := range bases {
		sb := scopeBase{b, realPath(b)}
		scope = append(scope, sb)
		keys = append(keys, baseKey(sb.given), baseKey(sb.real))
	}
	type cand struct {
		path, id, proj string
		when           time.Time
		src            int
	}
	srcs := sessionSources(c)
	var cands []cand
	for si, src := range srcs {
		projects := filepath.Join(src.dir, "projects")
		dirs, _ := os.ReadDir(projects)
		for _, d := range dirs {
			if !d.IsDir() || !slices.ContainsFunc(keys, func(k string) bool { return keyMatches(d.Name(), k) }) {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(projects, d.Name()))
			for _, f := range files {
				id, ok := strings.CutSuffix(f.Name(), ".jsonl")
				if !ok || !plainID(id) || !f.Type().IsRegular() { // the ID becomes an argv value
					continue
				}
				if info, err := f.Info(); err == nil {
					cands = append(cands, cand{filepath.Join(projects, d.Name(), f.Name()), id, d.Name(), info.ModTime(), si})
				}
			}
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if n := b.when.Compare(a.when); n != 0 {
			return n
		}
		return strings.Compare(a.path, b.path)
	})
	cut = len(cands) > maxSessionFiles
	cands = cands[:min(len(cands), maxSessionFiles)]

	resolve := projectResolver(scope)
	found := make([]*Session, len(cands))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(8, len(cands)) {
		wg.Go(func() {
			for i := range next {
				cd := cands[i]
				cwd, title := readSession(cd.path)
				if cwd == "" {
					continue
				}
				usable := filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd // no "..": stay in scope
				dir, ok, hint := "", false, ""
				if usable {
					hint = filepath.Base(cwd)
				}
				if !usable || !keyIs(cd.proj, projectKey(cwd)) { // moved since: the project dir names where it resumes
					dir, ok = resolve(cd.proj, hint)
				}
				if !ok && usable {
					dir, ok = inScope(cwd, scope)
				}
				if !ok {
					continue
				}
				found[i] = &Session{ID: cd.id, Dir: dir, Title: title, When: cd.when,
					Account: srcs[cd.src].owner(c, dir)}
			}
		})
	}
	for i := range cands {
		next <- i
	}
	close(next)
	wg.Wait()
	for _, s := range found {
		if s != nil {
			list = append(list, *s)
		}
	}
	return list, cut
}

// readSession returns a session file's first recorded cwd and its title. It
// reads at most sessionChunk bytes from each end of the file.
func readSession(path string) (cwd, title string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", ""
	}
	var chunks [][]byte
	if size := st.Size(); size <= 2*sessionChunk {
		buf, _ := io.ReadAll(io.LimitReader(f, 2*sessionChunk))
		chunks = [][]byte{buf}
	} else {
		head := make([]byte, sessionChunk)
		n, _ := f.ReadAt(head, 0)
		head = head[:bytes.LastIndexByte(head[:n], '\n')+1] // drop the cut last line
		tail := make([]byte, sessionChunk+1)                // +1: a whole first line starts after a newline
		n, _ = f.ReadAt(tail, size-int64(len(tail)))
		if i := bytes.IndexByte(tail[:n], '\n'); i >= 0 {
			tail = tail[i+1 : n] // drop the cut first line
		} else {
			tail = nil
		}
		chunks = [][]byte{head, tail}
	}
	var s sessionInfo
	for _, c := range chunks {
		for line := range bytes.SplitSeq(c, []byte("\n")) {
			s.add(line)
		}
	}
	return s.cwd, s.title()
}

// sessionInfo collects what a session list shows from its records.
type sessionInfo struct {
	cwd, custom, ai, prompt, first string
}

// add reads one JSON record, decoding only the fields it needs.
func (s *sessionInfo) add(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var r struct {
		Type        string `json:"type"`
		Cwd         string `json:"cwd"`
		CustomTitle string `json:"customTitle"`
		AITitle     string `json:"aiTitle"`
		LastPrompt  string `json:"lastPrompt"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	if s.cwd == "" {
		s.cwd = r.Cwd
	}
	set := func(dst *string, v string) {
		if v = oneLine(v); v != "" {
			*dst = v
		}
	}
	switch r.Type {
	case "custom-title":
		set(&s.custom, r.CustomTitle)
	case "ai-title":
		set(&s.ai, r.AITitle)
	case "last-prompt":
		set(&s.prompt, r.LastPrompt)
	case "user":
		if s.first != "" || r.IsMeta || r.IsSidechain {
			return
		}
		var u struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		var text string
		if json.Unmarshal(line, &u) == nil && json.Unmarshal(u.Message.Content, &text) == nil {
			set(&s.first, text)
		}
	}
}

func (s *sessionInfo) title() string {
	for _, t := range []string{s.custom, s.ai, s.prompt, s.first} {
		if t != "" {
			return t
		}
	}
	return "(untitled)"
}

// oneLine collapses whitespace runs to single spaces and caps s at
// maxTitleLen runes.
func oneLine(s string) string {
	for i := range s { // bound the work on huge prompts
		if i > 8*maxTitleLen {
			s = s[:i]
			break
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxTitleLen {
		s = string(r[:maxTitleLen])
	}
	return s
}
