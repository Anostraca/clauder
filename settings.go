//go:build unix

package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type setSub int

const (
	setList setSub = iota
	setAddPath
	setAddName
	setRename
	setDelete
)

type settingsState struct {
	sel, top int
	sub      setSub
	path     string // validated path while adding
	err      string
	comp     *completion
	cands    []string
}

// completion is an active Tab cycle over directory candidates.
type completion struct {
	dir   string // typed directory part, kept verbatim (may start with ~)
	cands []string
	idx   int
}

func (m *model) openSettings() {
	m.mode = modeSettings
	m.set = settingsState{}
}

func (m *model) leaveSettings() {
	m.mode = modeBrowse
	m.cur = ""
	m.load("")
}

func (m *model) saveSettings(ok string) {
	if err := m.cfg.Save(); err != nil {
		m.set.err = "save failed: " + err.Error()
		return
	}
	m.msg = ok
}

func (m *model) settingsKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	s := &m.set
	if s.sub != setList {
		return m.settingsInputKey(k)
	}
	m.msg, s.err = "", ""
	n := len(m.cfg.Roots)
	switch key {
	case "esc", "q":
		m.leaveSettings()
	case "up", "k":
		if n > 0 {
			s.sel = (s.sel - 1 + n) % n
		}
	case "down", "j":
		if n > 0 {
			s.sel = (s.sel + 1) % n
		}
	case "a":
		s.sub = setAddPath
		m.textInput("path: ", "", "~/projects")
	case "r":
		if n > 0 {
			s.sub = setRename
			m.textInput("name: ", m.cfg.Roots[s.sel].Name, "")
		}
	case "d", "x":
		if n > 0 {
			s.sub = setDelete
		}
	case "c":
		if n > 0 {
			next := palette[(slices.Index(palette, m.cfg.RootColor(s.sel))+1)%len(palette)]
			m.cfg.Roots[s.sel].Color = next
			m.saveSettings("")
		}
	case "J", "shift+down":
		if s.sel+1 < n {
			m.swapRoots(s.sel, s.sel+1)
			s.sel++
		}
	case "K", "shift+up":
		if s.sel > 0 {
			m.swapRoots(s.sel, s.sel-1)
			s.sel--
		}
	}
	return nil
}

// swapRoots reorders two roots, pinning their colours so they don't change.
func (m *model) swapRoots(i, j int) {
	r := m.cfg.Roots
	r[i].Color, r[j].Color = m.cfg.RootColor(i), m.cfg.RootColor(j)
	r[i], r[j] = r[j], r[i]
	m.saveSettings("")
}

func (m *model) settingsInputKey(k tea.KeyMsg) tea.Cmd {
	s := &m.set
	key := k.String()
	if s.sub == setDelete {
		if key == "y" || key == "Y" {
			name := m.cfg.Roots[s.sel].Name
			m.cfg.Roots = slices.Delete(m.cfg.Roots, s.sel, s.sel+1)
			s.sel = min(s.sel, max(len(m.cfg.Roots)-1, 0))
			m.saveSettings(dimS.Render("deleted root " + sanitize(name, false)))
		}
		s.sub = setList
		return nil
	}
	if key != "tab" {
		s.comp, s.cands = nil, nil
	}
	switch key {
	case "esc":
		s.sub, s.err = setList, ""
		return nil
	case "tab":
		if s.sub == setAddPath {
			v, comp, cands := completeDir(m.input.Value(), s.comp)
			s.comp, s.cands = comp, cands
			m.input.SetValue(v)
			m.input.CursorEnd()
		}
		return nil
	case "enter":
		m.settingsSubmit()
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	s.err = ""
	return cmd
}

func (m *model) settingsSubmit() {
	s := &m.set
	v := m.input.Value()
	switch s.sub {
	case setAddPath:
		p, err := filepath.Abs(expandPath(strings.TrimSpace(v)))
		switch {
		case strings.TrimSpace(v) == "":
			s.err = "path is empty"
		case err != nil:
			s.err = err.Error()
		case !isDir(p):
			s.err = tildePath(p) + " is not a directory"
		case m.cfg.rootIndex(p) >= 0:
			s.err = tildePath(p) + " is already a root"
		default:
			s.path, s.sub, s.err = p, setAddName, ""
			m.textInput("name: ", m.cfg.uniqueName(defaultRootName(p)), "")
		}
	case setAddName:
		if err := validRootName(m.cfg, v, -1); err != nil {
			s.err = err.Error()
			return
		}
		if err := m.cfg.AddRoot(v, s.path); err != nil {
			s.err = "save failed: " + err.Error()
			return
		}
		s.sel, s.sub = len(m.cfg.Roots)-1, setList
		m.msg = okS.Render("added root " + sanitize(v, false))
	case setRename:
		if err := validRootName(m.cfg, v, s.sel); err != nil {
			s.err = err.Error()
			return
		}
		m.cfg.Roots[s.sel].Name = v
		s.sub = setList
		m.saveSettings("")
	}
}

// completeDir completes the directory path in v. A single candidate is
// completed with a trailing "/", several to their longest common prefix; a
// Tab that makes no progress starts cycling through the candidates.
func completeDir(v string, c *completion) (string, *completion, []string) {
	if c != nil {
		c.idx = (c.idx + 1) % len(c.cands)
		return c.dir + c.cands[c.idx], c, c.cands
	}
	if v == "~" {
		return "~/", nil, nil
	}
	i := strings.LastIndex(v, "/")
	dirPart, prefix := v[:i+1], v[i+1:]
	dir := "."
	if dirPart != "" {
		dir = expandPath(dirPart)
	}
	var cands []string
	for _, name := range listDirs(dir) {
		if strings.HasPrefix(name, prefix) {
			cands = append(cands, name)
		}
	}
	switch len(cands) {
	case 0:
		return v, nil, nil
	case 1:
		return dirPart + cands[0] + "/", nil, nil
	}
	if lcp := commonPrefix(cands); len(lcp) > len(prefix) {
		return dirPart + lcp, nil, cands
	}
	c = &completion{dir: dirPart, cands: cands}
	return dirPart + cands[0], c, cands
}

func commonPrefix(ss []string) string {
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

func (m *model) viewSettings(b *strings.Builder) {
	s := &m.set
	b.WriteString("  " + boldS.Render("settings") + "  " + dimS.Render(safePath(m.cfg.file)) + "\n")
	width := 0
	for _, r := range m.cfg.Roots {
		width = max(width, len(r.Name))
	}
	m.list(b, len(m.cfg.Roots), s.sel, &s.top, m.settingsExtra(), func(i, _ int, selected bool) string {
		r := m.cfg.Roots[i]
		c := fg(m.cfg.RootColor(i))
		name := fmt.Sprintf("%-*s", width, sanitize(r.Name, false))
		line := c.Render("██") + " " + name + "  " + dimS.Render(safePath(r.Path))
		if selected {
			line = c.Render("██") + " " + boldS.Render(name) + "  " + dimS.Render(safePath(r.Path))
		}
		if !isDir(r.Path) {
			line += " " + dimS.Render("(missing)")
		}
		if selected {
			return "  " + c.Render("❯") + " " + line
		}
		return "    " + line
	})
	if len(m.cfg.Roots) == 0 {
		b.WriteString("    " + dimS.Render("(no roots)") + "\n")
	}
	switch s.sub {
	case setAddPath, setAddName, setRename:
		title := map[setSub]string{setAddPath: "add root", setAddName: "add root", setRename: "rename"}[s.sub]
		b.WriteString("\n  " + dimS.Render(title+" —") + " " + m.input.View() + "\n")
		if len(s.cands) > 0 {
			b.WriteString("  " + dimS.Render(sanitize(truncList(s.cands, 8), false)) + "\n")
		}
	case setDelete:
		b.WriteString("\n  " + warnS.Render(fmt.Sprintf("delete root %s? y/n", sanitize(m.cfg.Roots[s.sel].Name, false))) + "\n")
	}
	if s.err != "" {
		b.WriteString("  " + errS.Render(sanitize(s.err, false)) + "\n")
	}
	m.footer(b)
}

func (m *model) settingsHint() string {
	switch m.set.sub {
	case setAddPath:
		return "tab complete · ⏎ next · esc cancel"
	case setAddName, setRename:
		return "⏎ save · esc cancel"
	case setDelete:
		return "y delete · any other key cancels"
	}
	return "↑↓ move · a add · r rename · d delete · c colour · J/K reorder · esc back"
}

// settingsExtra counts the lines below the root list (input, completions,
// prompt, error) so the list shrinks instead of pushing the header off screen.
func (m *model) settingsExtra() int {
	s := &m.set
	n := 0
	switch s.sub {
	case setAddPath, setAddName, setRename, setDelete:
		n += 2
	}
	if len(s.cands) > 0 {
		n++
	}
	if s.err != "" {
		n++
	}
	return n
}

func truncList(ss []string, n int) string {
	if len(ss) <= n {
		return strings.Join(ss, "  ")
	}
	return strings.Join(ss[:n], "  ") + fmt.Sprintf("  … +%d", len(ss)-n)
}
