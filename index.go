//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sahilm/fuzzy"
)

// hidden reports names the browser never shows: dotdirs and Synology's
// @eaDir / #recycle style entries.
func hidden(name string) bool {
	return name == "" || name[0] == '.' || name[0] == '@' || name[0] == '#'
}

func ignored(name string, ignore []string) bool {
	for _, pat := range ignore {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// listDirs returns the visible subdirectory names of dir, sorted
// case-insensitively. Symlinks are not followed.
func listDirs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && !hidden(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := strings.ToLower(names[i]), strings.ToLower(names[j])
		if a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	return names
}

// hasKids reports whether dir has at least one visible subdirectory.
func hasKids(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		ents, err := f.ReadDir(64)
		for _, e := range ents {
			if e.IsDir() && !hidden(e.Name()) {
				return true
			}
		}
		if err != nil || len(ents) == 0 {
			return false
		}
	}
}

// Entry is one folder found by a search walk.
type Entry struct {
	Path    string // absolute
	Display string // root/rel/path
	Color   int    // colour of the root part, 0 = none
	Prefix  int    // byte length of the root part of Display
}

// Base is a search starting point.
type Base struct {
	Dir    string
	Label  string // display prefix
	Color  int
	Prefix int  // byte length of the coloured root part of Label
	Self   bool // include the base itself as a result
}

// walk lists folders under bases with a fixed pool of workers, up to
// maxDepth levels below each base, sending batches as they are found. The
// channel closes when the walk finishes or ctx is cancelled.
func walk(ctx context.Context, bases []Base, maxDepth int, ignore []string) <-chan []Entry {
	type job struct {
		b          *Base
		dir, label string
		depth      int
	}
	out := make(chan []Entry, 64)
	send := func(b []Entry) {
		select {
		case out <- b:
		case <-ctx.Done():
		}
	}

	// queue of directories to read; pending counts queued + in-flight jobs
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	var queue []job
	pending := 0
	push := func(j job) {
		mu.Lock()
		queue = append(queue, j)
		pending++
		mu.Unlock()
		cond.Signal()
	}
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		cond.Broadcast()
		mu.Unlock()
	})

	var batch []Entry
	for i := range bases {
		b := &bases[i]
		if b.Self && isDir(b.Dir) {
			batch = append(batch, Entry{Path: b.Dir, Display: b.Label, Color: b.Color, Prefix: b.Prefix})
		}
		push(job{b, b.Dir, b.Label, 0})
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				for len(queue) == 0 && pending > 0 && ctx.Err() == nil {
					cond.Wait()
				}
				if len(queue) == 0 || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				j := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				mu.Unlock()

				var found []Entry
				ents, _ := os.ReadDir(j.dir)
				for _, e := range ents {
					name := e.Name()
					if !e.IsDir() || hidden(name) || ignored(name, ignore) {
						continue
					}
					p, l := filepath.Join(j.dir, name), j.label+"/"+name
					found = append(found, Entry{Path: p, Display: l, Color: j.b.Color, Prefix: j.b.Prefix})
					if j.depth+1 < maxDepth {
						push(job{j.b, p, l, j.depth + 1})
					}
				}
				if len(found) > 0 {
					send(found)
				}
				mu.Lock()
				pending--
				if pending == 0 {
					cond.Broadcast()
				}
				mu.Unlock()
			}
		}()
	}
	go func() {
		if len(batch) > 0 {
			send(batch)
		}
		wg.Wait()
		stop()
		close(out)
	}()
	return out
}

// Result is a ranked search match.
type Result struct {
	Entry
	Fav   bool
	Score int
	Idx   []int // matched byte offsets in Display
}

type entrySource []Entry

func (s entrySource) String(i int) string { return s[i].Display }
func (s entrySource) Len() int            { return len(s) }

// rank filters entries by query and orders them: favorites first, then fuzzy
// score, then shorter path.
func rank(entries []Entry, query string, favs map[string]bool) []Result {
	var res []Result
	if query == "" {
		res = make([]Result, len(entries))
		for i, e := range entries {
			res[i] = Result{Entry: e, Fav: favs[e.Path]}
		}
	} else {
		for _, m := range fuzzy.FindFromNoSort(query, entrySource(entries)) {
			e := entries[m.Index]
			res = append(res, Result{Entry: e, Fav: favs[e.Path], Score: m.Score, Idx: m.MatchedIndexes})
		}
	}
	sort.Slice(res, func(i, j int) bool {
		a, b := res[i], res[j]
		if a.Fav != b.Fav {
			return a.Fav
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if len(a.Display) != len(b.Display) {
			return len(a.Display) < len(b.Display)
		}
		return a.Display < b.Display
	})
	return res
}

// rootBases returns a search base for every existing root.
func rootBases(c *Config) []Base {
	var bs []Base
	for i, r := range c.Roots {
		if isDir(r.Path) {
			bs = append(bs, Base{Dir: r.Path, Label: r.Name, Color: c.RootColor(i), Prefix: len(r.Name), Self: true})
		}
	}
	return bs
}

// folderBase returns the search base for a folder inside the browser.
func folderBase(c *Config, dir string) Base {
	b := Base{Dir: dir, Label: c.label(dir)}
	if i, _ := c.locate(dir); i >= 0 {
		b.Color, b.Prefix = c.RootColor(i), len(c.Roots[i].Name)
	}
	return b
}

func favSet(c *Config) map[string]bool {
	m := make(map[string]bool, len(c.Favorites))
	for _, f := range c.Favorites {
		m[f] = true
	}
	return m
}

// collect drains a walk.
func collect(ch <-chan []Entry) []Entry {
	var all []Entry
	for b := range ch {
		all = append(all, b...)
	}
	return all
}
