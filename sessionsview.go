//go:build unix

package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// sessState is the recent-sessions list for a scope of folders.
type sessState struct {
	label    string
	back     mode // view to return to
	list     []Session
	sel, top int
	loading  bool
	gen      int
	cut      bool // only the newest maxSessionFiles files were read
}

type sessionsMsg struct {
	gen  int
	list []Session
	cut  bool
}

// openSessions lists the sessions recorded in bases in the background.
func (m *model) openSessions(bases []string, label string) tea.Cmd {
	gen := m.sess.gen + 1
	m.sess = sessState{label: label, back: m.mode, loading: true, gen: gen}
	m.mode = modeSessions
	c := &Config{Accounts: slices.Clone(m.cfg.Accounts), DefaultAccount: m.cfg.DefaultAccount,
		FolderAccounts: maps.Clone(m.cfg.FolderAccounts)} // the scan must not race config edits
	return func() tea.Msg {
		list, cut := findSessions(c, bases)
		return sessionsMsg{gen, list, cut}
	}
}

// browseSessions opens the sessions of the current folder, or of all roots
// on the top screen.
func (m *model) browseSessions() tea.Cmd {
	if m.cur != "" {
		return m.openSessions([]string{m.cur}, m.cfg.label(m.cur))
	}
	var bases []string
	for _, b := range rootBases(m.cfg) {
		bases = append(bases, b.Dir)
	}
	return m.openSessions(bases, "all roots")
}

func (m *model) favSessions() tea.Cmd {
	var bases []string
	for _, f := range m.cfg.Favorites {
		if isDir(f) {
			bases = append(bases, f)
		}
	}
	return m.openSessions(bases, "favorites")
}

func (m *model) onSessions(msg sessionsMsg) {
	if msg.gen != m.sess.gen {
		return
	}
	m.sess.list, m.sess.cut, m.sess.loading = msg.list, msg.cut, false
}

func (m *model) leaveSessions() {
	m.sess.gen++ // drop a late result
	m.mode = m.sess.back
}

func (m *model) sessionsKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	s := &m.sess
	n := len(s.list)
	switch k.String() {
	case "q":
		return m.quit(1)
	case "esc", "left":
		m.leaveSessions()
	case "up", "k":
		if n > 0 {
			s.sel = (s.sel - 1 + n) % n
		}
	case "down", "j":
		if n > 0 {
			s.sel = (s.sel + 1) % n
		}
	case "enter", "right":
		if n == 0 {
			return nil
		}
		ss := s.list[s.sel]
		if !isDir(ss.Dir) {
			m.msg = errS.Render("not found: " + safePath(ss.Dir))
			return nil
		}
		if k.String() == "right" {
			m.sess.gen++
			m.enter(ss.Dir, false)
			return nil
		}
		m.resume, m.chosenAcct = ss.ID, ss.Account
		return m.finish(ss.Dir)
	}
	return nil
}

// ago is a short age: now, 5m, 3h, 2d, then a date.
func ago(t, now time.Time) string {
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("2006")
	}
}

func (m *model) viewSessions(b *strings.Builder) {
	s := &m.sess
	b.WriteString("  " + boldS.Render("sessions") + dimS.Render(" in ") + sanitize(s.label, false) + "\n")
	status := "loading…"
	if !s.loading {
		status = fmt.Sprintf("%d sessions", len(s.list))
		if len(s.list) == 1 {
			status = "1 session"
		}
		if s.cut {
			status += fmt.Sprintf(" · only the newest %d files read", maxSessionFiles)
		}
	}
	b.WriteString("  " + dimS.Render(status) + "\n")
	width := m.width
	if width == 0 {
		width = 80
	}
	now := time.Now()
	m.list(b, len(s.list), s.sel, &s.top, 1, func(i, _ int, selected bool) string {
		ss := s.list[i]
		missing := !isDir(ss.Dir)
		tag := ""
		if m.acctExtra() > 0 && ss.Account != "" {
			tag = " " + dimS.Render("["+sanitize(ss.Account, false)+"]")
		}
		if missing {
			tag = " " + dimS.Render("(missing)") + tag
		}
		avail := width - 4 - 7 - lipgloss.Width(tag) // marker, age column
		folder, color := m.sessFolder(ss.Dir, max(avail, 1), selected, missing)
		text := dimS.Render(fmt.Sprintf("%-6s", ago(ss.When, now))) + " " + folder
		if w := avail - lipgloss.Width(folder) - 2; w > 1 {
			title := lipgloss.NewStyle()
			if missing {
				title = dimS
			}
			if selected {
				title = title.Bold(true)
			}
			text += "  " + title.Render(truncWidth(sanitize(ss.Title, false), w))
		}
		text += tag
		if selected {
			return "  " + fg(color).Render("❯") + " " + text
		}
		return "    " + text
	})
	if len(s.list) == 0 && !s.loading {
		b.WriteString("    " + dimS.Render("(no sessions)") + "\n")
	}
	m.footer(b)
}

// sessFolder renders dir's label in at most w cells with the root part
// coloured; it also returns the root colour.
func (m *model) sessFolder(dir string, w int, selected, missing bool) (string, int) {
	label := truncWidth(sanitize(m.cfg.label(dir), false), w)
	root, color := "", 0
	if i, _ := m.cfg.locate(dir); i >= 0 {
		root, color = sanitize(m.cfg.Roots[i].Name, false), m.cfg.RootColor(i)
	}
	head, rest := fg(color), lipgloss.NewStyle()
	if missing {
		head, rest = dimS, dimS
	}
	if selected {
		head, rest = head.Bold(true), rest.Bold(true)
	}
	if root == "" || !strings.HasPrefix(label, root) {
		return rest.Render(label), color
	}
	return head.Render(root) + rest.Render(label[len(root):]), color
}
