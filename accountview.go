//go:build unix

package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// acctState is the account selector for one folder.
type acctState struct {
	target   string
	back     mode // view to return to
	sel, top int
}

// openAccounts opens the account selector for folder p.
func (m *model) openAccounts(p string) {
	if len(m.cfg.Accounts) == 0 {
		m.msg = dimS.Render("no accounts yet — add them in settings")
		return
	}
	sel := 0
	if acc, _ := m.cfg.ResolveAccount(p); acc != nil {
		sel = m.cfg.accountIndex(acc.Name)
	}
	if i := m.cfg.accountIndex(m.acctFlag); i >= 0 {
		sel = i
	}
	m.acct = acctState{target: p, back: m.mode, sel: sel}
	m.mode = modeAccount
}

func (m *model) accountKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	a := &m.acct
	n := len(m.cfg.Accounts)
	if n == 0 {
		m.mode = a.back
		return nil
	}
	a.sel = min(a.sel, n-1)
	name := m.cfg.Accounts[a.sel].Name
	label := m.cfg.label(a.target)
	switch k.String() {
	case "esc", "q", "left":
		m.mode = a.back
	case "up", "k":
		a.sel = (a.sel - 1 + n) % n
	case "down", "j":
		a.sel = (a.sel + 1) % n
	case "enter":
		if !isDir(a.target) {
			m.msg = errS.Render("not found: " + safePath(a.target))
			return nil
		}
		m.chosenAcct = name
		return m.finish(a.target)
	case "s":
		if err := m.cfg.SetFolderAccount(a.target, name); err != nil {
			m.msg = errS.Render("save failed: " + sanitize(err.Error(), false))
			return nil
		}
		m.mode = a.back
		m.msg = okS.Render(sanitize(label+" → "+name, false))
	case "c":
		if _, ok := m.cfg.FolderAccounts[a.target]; !ok {
			m.msg = dimS.Render(sanitize(label, false) + " has no account of its own")
			return nil
		}
		if err := m.cfg.ClearFolderAccount(a.target); err != nil {
			m.msg = errS.Render("save failed: " + sanitize(err.Error(), false))
			return nil
		}
		m.mode = a.back
		m.msg = dimS.Render(sanitize(label, false) + " inherits its account again")
	}
	return nil
}

func (m *model) viewAccount(b *strings.Builder) {
	a := &m.acct
	b.WriteString("  " + boldS.Render("account") + dimS.Render(" for ") + sanitize(m.cfg.label(a.target), false) + "\n")
	eff, from := m.cfg.ResolveAccount(a.target)
	own := m.cfg.folderAccount(a.target)
	width := 0
	for _, acc := range m.cfg.Accounts {
		width = max(width, len(sanitize(acc.Name, false)))
	}
	m.list(b, len(m.cfg.Accounts), a.sel, &a.top, 0, func(i, _ int, selected bool) string {
		acc := &m.cfg.Accounts[i]
		name := fmt.Sprintf("%-*s", width, sanitize(acc.Name, false))
		if selected {
			name = boldS.Render(name)
		}
		line := name + "  " + dimS.Render(sanitize(infoFor(acc).summary(), false))
		if strings.EqualFold(acc.Name, m.cfg.DefaultAccount) {
			line += " " + okS.Render("default")
		}
		switch {
		case own == acc:
			line += " " + warnS.Render("set here")
		case eff == acc && from != "":
			line += " " + warnS.Render("from "+sanitize(m.cfg.label(from), false))
		}
		if selected {
			return "  ❯ " + line
		}
		return "    " + line
	})
	m.footer(b)
}

// acctExtra is the account line under browse and search lists (0 or 1).
func (m *model) acctExtra() int {
	if m.cfg != nil && len(m.cfg.Accounts) > 0 {
		return 1
	}
	return 0
}

// accountLine is the dim line naming the account a run in p would use
// ("" when no accounts are configured; a blank line when p is "").
func (m *model) accountLine(p string) string {
	if m.acctExtra() == 0 {
		return ""
	}
	if p == "" {
		return "\n"
	}
	s := "account: none"
	if acc := m.cfg.Account(m.acctFlag); acc != nil {
		s = "account: " + accountDesc(acc) + " · --account"
	} else if acc, from := m.cfg.ResolveAccount(p); acc != nil {
		s = "account: " + accountDesc(acc)
		switch from {
		case p:
			s += " · set here"
		case "":
			s += " · default"
		default:
			s += " · from " + m.cfg.label(from)
		}
	}
	width := m.width
	if width == 0 {
		width = 80
	}
	return "  " + dimS.Render(truncWidth(sanitize(s, false), width-2)) + "\n"
}

// acctTag marks a folder with an explicit account assignment.
func (m *model) acctTag(p string) string {
	if a := m.cfg.folderAccount(p); a != nil {
		return " " + dimS.Render("["+sanitize(a.Name, false)+"]")
	}
	return ""
}

// acctHint is s when accounts are configured, for the key hints.
func (m *model) acctHint(s string) string {
	if m.acctExtra() == 0 {
		return ""
	}
	return s
}
