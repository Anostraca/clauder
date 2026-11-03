//go:build unix

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type syncState struct {
	target, label string
	results       []RepoResult
	idx           []int // event index -> results index of the running batch
	sel, top      int
	gen, frame    int
	discovering   bool
	running       bool
	ticking       bool // a spinner tick chain is active
	detail        bool
	err           string
	note          string // progress note from runSync, e.g. "connecting to host…"
	cancel        context.CancelFunc
	ch            <-chan SyncEvent
}

type discoverMsg struct {
	gen   int
	repos []Repo
	err   error
}

type syncMsg struct {
	gen  int
	ev   SyncEvent
	done bool
}

type tickMsg struct{ gen int }

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *model) tick() tea.Cmd {
	m.sync.ticking = true
	gen := m.sync.gen
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{gen} })
}

func (m *model) onTick(t tickMsg) tea.Cmd {
	if t.gen != m.sync.gen {
		return nil
	}
	if !m.sync.running && !m.sync.discovering {
		m.sync.ticking = false
		return nil
	}
	m.sync.frame++
	return m.tick()
}

func (m *model) startSync(path string) tea.Cmd {
	if m.sync.cancel != nil {
		m.sync.cancel()
	}
	gen := m.sync.gen + 1
	m.sync = syncState{target: path, label: m.cfg.label(path), discovering: true, gen: gen}
	m.mode = modeSync
	ignore, exclude := m.cfg.Search.Ignore, m.cfg.Git.Exclude
	return tea.Batch(func() tea.Msg {
		repos, err := discover(path, ignore, exclude)
		return discoverMsg{gen, repos, err}
	}, m.tick())
}

func (m *model) onDiscover(d discoverMsg) tea.Cmd {
	s := &m.sync
	if d.gen != s.gen {
		return nil
	}
	s.discovering = false
	switch {
	case d.err != nil:
		s.err = d.err.Error()
		return nil
	case len(d.repos) == 0:
		s.err = "no git repositories found"
		return nil
	}
	s.results = make([]RepoResult, len(d.repos))
	all := make([]int, len(d.repos))
	for i, r := range d.repos {
		s.results[i] = RepoResult{Repo: r}
		all[i] = i
	}
	return m.runJobs(all, false, true)
}

// runJobs syncs the given result rows. rediscover re-reads .meta files after
// the meta repos are pulled (only for a full run, where idx is the identity).
func (m *model) runJobs(indexes []int, clone, rediscover bool) tea.Cmd {
	s := &m.sync
	repos := make([]Repo, len(indexes))
	for k, i := range indexes {
		repos[k] = s.results[i].Repo
		s.results[i].Status = StPending
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.idx, s.running = cancel, indexes, true
	o := SyncOpts{Jobs: m.cfg.Git.Jobs, Timeout: m.cfg.GitTimeout(), Clone: clone}
	if rediscover {
		target, ignore, exclude := s.target, m.cfg.Search.Ignore, m.cfg.Git.Exclude
		o.Rediscover = func() ([]Repo, error) { return discover(target, ignore, exclude) }
	}
	s.ch = runSync(ctx, repos, o)
	m.jobs = append(m.jobs, s.ch)
	if s.ticking {
		return waitSync(s.gen, s.ch)
	}
	return tea.Batch(waitSync(s.gen, s.ch), m.tick())
}

func waitSync(gen int, ch <-chan SyncEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return syncMsg{gen: gen, ev: ev, done: !ok}
	}
}

func (m *model) onSync(msg syncMsg) tea.Cmd {
	s := &m.sync
	if msg.gen != s.gen {
		return nil
	}
	ev := msg.ev
	switch {
	case msg.done:
		s.running, s.note = false, ""
		sortResults(s.results)
		return nil
	case ev.Note != "":
		s.note = ev.Note
	case ev.Repos != nil: // .meta re-read: keep known statuses
		s.note = ""
		prev := map[string]RepoResult{}
		for _, r := range s.results {
			prev[r.Dir] = r
		}
		s.results, s.idx = make([]RepoResult, len(ev.Repos)), make([]int, len(ev.Repos))
		for i, r := range ev.Repos {
			s.results[i], s.idx[i] = RepoResult{Repo: r}, i
			if p, ok := prev[r.Dir]; ok {
				s.results[i].Status = p.Status
			}
		}
		s.sel = min(s.sel, max(len(s.results)-1, 0))
	default:
		s.note = ""
		s.results[s.idx[ev.Index]] = ev.Result
	}
	return waitSync(s.gen, s.ch)
}

func (m *model) leaveSync() {
	if m.sync.cancel != nil {
		m.sync.cancel()
	}
	m.sync.gen++ // drop late messages
	m.mode = modeBrowse
	back := ""
	if it, ok := m.selected(); ok {
		back = it.path
	}
	m.load(back)
}

func (m *model) syncKey(k tea.KeyMsg) tea.Cmd {
	s := &m.sync
	key := k.String()
	m.msg = ""
	if s.detail {
		if key == "esc" || key == "enter" || key == "left" || key == "q" {
			s.detail = false
		}
		return nil
	}
	n := len(s.results)
	switch key {
	case "esc":
		m.leaveSync()
	case "q":
		return m.quit(1)
	case "up", "k":
		if n > 0 {
			s.sel = (s.sel - 1 + n) % n
		}
	case "down", "j":
		if n > 0 {
			s.sel = (s.sel + 1) % n
		}
	case "enter":
		if n > 0 {
			s.detail = true
		}
	case "c":
		if s.running || s.discovering {
			return nil
		}
		var missing []int
		for i, r := range s.results {
			if r.Status == StMissing {
				missing = append(missing, i)
			}
		}
		if len(missing) == 0 {
			m.msg = dimS.Render("nothing to clone")
			return nil
		}
		return m.runJobs(missing, true, false)
	}
	return nil
}

func (m *model) syncRow(r RepoResult) string {
	var icon string
	switch r.Status {
	case StPending:
		icon = dimS.Render("·")
	case StRunning:
		icon = spinFrames[m.sync.frame%len(spinFrames)]
	case StUpdated, StCloned:
		icon = okS.Render("✓")
	case StUpToDate:
		icon = dimS.Render("=")
	case StFailed:
		icon = errS.Render("✗")
	case StMissing:
		icon = warnS.Render("∅")
	}
	s := icon + " " + r.Rel
	switch r.Status {
	case StFailed:
		s += dimS.Render(": " + r.Err)
	case StMissing:
		s += " " + dimS.Render("(missing)")
	case StCloned:
		s += " " + dimS.Render("(cloned)")
	}
	if r.Dirty {
		s += " " + warnS.Render("⚠ dirty")
	}
	return s
}

func (m *model) viewSync(b *strings.Builder) {
	s := &m.sync
	b.WriteString("  " + boldS.Render("pull") + " " + s.label + "\n")
	if s.detail && s.sel < len(s.results) {
		r := s.results[s.sel]
		b.WriteString("  " + m.syncRow(r) + "\n\n")
		out := strings.TrimSpace(r.Output)
		if out == "" {
			out = r.Err
		}
		if out == "" {
			out = "(no output)"
		}
		lines := strings.Split(out, "\n")
		if limit := m.rows(len(lines), 1); len(lines) > limit {
			lines = append(lines[:limit], fmt.Sprintf("… %d more lines", len(lines)-limit))
		}
		for _, l := range lines {
			b.WriteString("    " + dimS.Render(l) + "\n")
		}
		m.footer(b)
		return
	}
	switch {
	case s.discovering:
		b.WriteString("  " + dimS.Render(spinFrames[s.frame%len(spinFrames)]+" discovering repositories…") + "\n")
	case s.err != "":
		b.WriteString("  " + errS.Render(s.err) + "\n")
	case s.note != "":
		b.WriteString("  " + dimS.Render(spinFrames[s.frame%len(spinFrames)]+" "+s.note) + "\n")
	default:
		status := summarize(s.results)
		if s.running {
			done := 0
			for _, r := range s.results {
				if r.Status != StPending && r.Status != StRunning {
					done++
				}
			}
			status = fmt.Sprintf("%d/%d · %s", done, len(s.results), status)
		}
		b.WriteString("  " + dimS.Render(status) + "\n")
	}
	m.list(b, len(s.results), s.sel, &s.top, 1, func(i, _ int, selected bool) string {
		row := m.syncRow(s.results[i])
		if selected {
			return "  ❯ " + row
		}
		return "    " + row
	})
	m.footer(b)
}

// waitJobs waits (bounded) for cancelled syncs to wind down, so git is not
// orphaned and ssh masters are closed before clauder exits or execs.
func (m *model) waitJobs(limit time.Duration) {
	deadline := time.After(limit)
	for _, ch := range m.jobs {
		for open := true; open; {
			select {
			case _, open = <-ch:
			case <-deadline:
				return
			}
		}
	}
}
