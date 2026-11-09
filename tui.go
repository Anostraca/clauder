//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type mode int

const (
	modeBrowse mode = iota
	modeFavs
	modeSearch
	modeNew
	modeSettings
	modeSync
	modeAccount
)

type startOpts struct {
	mode    mode
	query   string
	entries []Entry // preloaded search entries (from --find)
	msg     string
	account string // --account override
}

type item struct {
	label, path   string
	color         int
	kids, missing bool
	fav           bool // favorited folder: shows a ★ marker
	favEntry      bool // the "★ favorites" pseudo-entry on the top screen
}

// navCtx is the root (or favorite outside any root) being browsed.
type navCtx struct {
	label string
	color int
	base  string
}

type model struct {
	cfg    *Config
	mode   mode
	width  int
	height int
	msg    string // one-shot status line

	// browse / favorites
	nav     navCtx
	cur     string // "" = top screen
	items   []item
	sel     int
	top     int
	favBack string

	// shared text input (search, new folder, settings)
	input textinput.Model

	// search
	entries    []Entry
	results    []Result
	rsel, rtop int
	walking    bool
	gen        int
	walkCh     <-chan []Entry
	cancelWalk context.CancelFunc
	walkStart  time.Time
	walkTook   time.Duration

	newErr string

	set  settingsState
	sync syncState
	acct acctState
	jobs []<-chan SyncEvent // every sync run started, for waitJobs

	chosen     string
	chosenAcct string // picked in the account selector; "" = resolve
	acctFlag   string // --account override for this run
	code       int
	quitting   bool
	initCmd    tea.Cmd
}

var (
	dimS      = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	boldS     = lipgloss.NewStyle().Bold(true)
	errS      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	starS     = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	okS       = lipgloss.NewStyle().Foreground(lipgloss.Color("71"))
	warnS     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	favEntryS = lipgloss.NewStyle().Foreground(lipgloss.Color("136"))
)

// initStyles rebuilds the styles for the renderer the TUI writes to.
func initStyles(r *lipgloss.Renderer) {
	lipgloss.SetDefaultRenderer(r)
	dimS = r.NewStyle().Foreground(lipgloss.Color("242"))
	boldS = r.NewStyle().Bold(true)
	errS = r.NewStyle().Foreground(lipgloss.Color("203"))
	starS = r.NewStyle().Foreground(lipgloss.Color("220"))
	okS = r.NewStyle().Foreground(lipgloss.Color("71"))
	warnS = r.NewStyle().Foreground(lipgloss.Color("214"))
	favEntryS = r.NewStyle().Foreground(lipgloss.Color("136"))
}

// safePath is a ~-path made safe to print.
func safePath(p string) string { return sanitize(tildePath(p), false) }

func fg(c int) lipgloss.Style {
	if c == 0 {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(strconv.Itoa(c)))
}

func newModel(cfg *Config, r *lipgloss.Renderer, s startOpts) *model {
	if r != nil {
		initStyles(r)
	}
	m := &model{cfg: cfg, msg: s.msg, acctFlag: s.account}
	m.load("")
	switch s.mode {
	case modeFavs:
		m.openFavs()
	case modeSettings:
		m.openSettings()
	case modeSearch:
		m.initCmd = m.startSearch(s.entries, s.query)
	}
	return m
}

func (m *model) Init() tea.Cmd { return m.initCmd }

func (m *model) quit(code int) tea.Cmd {
	m.code, m.quitting = code, true
	m.stopJobs()
	return tea.Quit
}

func (m *model) finish(path string) tea.Cmd {
	m.chosen = path
	return m.quit(0)
}

// stopJobs cancels any running search walk or git sync.
func (m *model) stopJobs() {
	m.stopWalk()
	if m.sync.cancel != nil {
		m.sync.cancel()
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case batchMsg:
		return m, m.onBatch(msg)
	case discoverMsg:
		return m, m.onDiscover(msg)
	case syncMsg:
		return m, m.onSync(msg)
	case tickMsg:
		return m, m.onTick(msg)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, m.quit(130)
		}
		switch m.mode {
		case modeBrowse:
			return m, m.browseKey(msg)
		case modeFavs:
			return m, m.favsKey(msg)
		case modeSearch:
			return m, m.searchKey(msg)
		case modeNew:
			return m, m.newKey(msg)
		case modeSettings:
			return m, m.settingsKey(msg)
		case modeSync:
			return m, m.syncKey(msg)
		case modeAccount:
			return m, m.accountKey(msg)
		}
	}
	return m, nil
}

// ---- browse ----------------------------------------------------------------

// favEntryPath identifies the "★ favorites" pseudo-entry for back-highlighting.
const favEntryPath = "\x00favorites"

func (m *model) favItem(p string) item {
	it := item{label: m.cfg.label(p), path: p}
	if i, _ := m.cfg.locate(p); i >= 0 {
		it.color = m.cfg.RootColor(i)
	}
	it.missing = !isDir(p)
	it.kids = !it.missing && hasKids(p)
	return it
}

// load reads the items for m.cur and highlights back if present.
func (m *model) load(back string) {
	m.items = nil
	if m.cur == "" {
		if len(m.cfg.Favorites) > 0 {
			m.items = append(m.items, item{label: "★ favorites", path: favEntryPath, favEntry: true, kids: true})
		}
		for i, r := range m.cfg.Roots {
			missing := !isDir(r.Path)
			m.items = append(m.items, item{label: r.Name, path: r.Path, color: m.cfg.RootColor(i),
				missing: missing, kids: !missing && hasKids(r.Path)})
		}
	} else {
		for _, name := range listDirs(m.cur) {
			p := filepath.Join(m.cur, name)
			m.items = append(m.items, item{label: name, path: p, kids: hasKids(p), fav: m.cfg.IsFavorite(p)})
		}
	}
	m.sel, m.top = 0, 0
	for i := range m.items {
		if m.items[i].path == back {
			m.sel = i
			break
		}
	}
}

func (m *model) selected() (item, bool) {
	if m.sel < len(m.items) {
		return m.items[m.sel], true
	}
	return item{}, false
}

func (m *model) navFor(p string) navCtx {
	if i, _ := m.cfg.locate(p); i >= 0 {
		return navCtx{m.cfg.Roots[i].Name, m.cfg.RootColor(i), m.cfg.Roots[i].Path}
	}
	return navCtx{tildePath(p), 0, p}
}

// enter opens dir in browse mode; keep retains the current root context.
func (m *model) enter(dir string, keep bool) {
	if !keep || m.cur == "" {
		m.nav = m.navFor(dir)
	}
	m.mode = modeBrowse
	m.cur = dir
	m.load("")
}

// open runs it (run, or when it has no subfolders) or navigates into it.
func (m *model) open(it item, run bool) tea.Cmd {
	if it.favEntry {
		m.openFavs()
		return nil
	}
	if it.missing {
		m.msg = errS.Render("not found: " + safePath(it.path))
		return nil
	}
	if run || !it.kids {
		return m.finish(it.path)
	}
	m.enter(it.path, m.mode == modeBrowse)
	return nil
}

func (m *model) up() {
	if m.cur == "" {
		return
	}
	back := m.cur
	if m.cur == m.nav.base || !strings.HasPrefix(m.cur, m.nav.base) {
		m.cur = ""
	} else {
		m.cur = filepath.Dir(m.cur)
	}
	m.load(back)
}

func (m *model) toggleFav(p string) {
	on, err := m.cfg.ToggleFavorite(p)
	switch {
	case err != nil:
		m.msg = errS.Render("save failed: " + sanitize(err.Error(), false))
	case on:
		m.msg = starS.Render("★") + " added " + sanitize(m.cfg.label(p), false)
	default:
		m.msg = dimS.Render("removed " + sanitize(m.cfg.label(p), false) + " from favorites")
	}
}

func (m *model) addRootFrom(it item) {
	switch {
	case it.missing:
		m.msg = errS.Render("not found: " + safePath(it.path))
	case m.cfg.rootIndex(it.path) >= 0:
		m.msg = dimS.Render(safePath(it.path) + " is already a root")
	default:
		name := m.cfg.uniqueName(defaultRootName(it.path))
		if err := m.cfg.AddRoot(name, it.path); err != nil {
			m.msg = errS.Render("save failed: " + sanitize(err.Error(), false))
			return
		}
		m.msg = okS.Render("added root " + sanitize(name, false))
		if m.cur == "" {
			m.load(it.path)
		}
	}
}

// pick maps a 1-9 key to a visible row index.
func (m *model) pick(key string) (int, bool) {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return 0, false
	}
	i := m.top + int(key[0]-'1')
	if i >= len(m.items) || i >= m.top+m.rows(len(m.items), m.acctExtra()) {
		return 0, false
	}
	return i, true
}

func (m *model) browseKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	n := len(m.items)
	it, ok := m.selected()
	switch key := k.String(); key {
	case "q", "esc":
		return m.quit(1)
	case "up", "k":
		if n > 0 {
			m.sel = (m.sel - 1 + n) % n
		}
	case "down", "j":
		if n > 0 {
			m.sel = (m.sel + 1) % n
		}
	case "enter":
		if ok {
			return m.open(it, true)
		}
		if m.cur != "" {
			return m.finish(m.cur)
		}
	case "right":
		if ok {
			return m.open(it, false)
		}
	case "left", "-", "backspace":
		m.up()
	case ".":
		if m.cur != "" {
			return m.finish(m.cur)
		}
	case "/":
		return m.startSearch(nil, "")
	case "n":
		if m.cur == "" {
			m.msg = dimS.Render("open a folder first to create a new folder in it")
		} else {
			m.startNew()
		}
	case "f":
		if ok && !it.favEntry {
			m.toggleFav(it.path)
			m.load(it.path)
		}
	case "F", "tab":
		m.openFavs()
	case "s":
		if m.cur == "" {
			m.openSettings()
		}
	case "+":
		if ok && !it.favEntry {
			m.addRootFrom(it)
		}
	case "p":
		if ok && !it.favEntry {
			return m.startSync(it.path)
		}
	case "u":
		switch {
		case ok && !it.favEntry:
			m.openAccounts(it.path)
		case !ok && m.cur != "":
			m.openAccounts(m.cur)
		}
	default:
		if i, ok := m.pick(key); ok {
			return m.open(m.items[i], false)
		}
	}
	return nil
}

// ---- favorites view --------------------------------------------------------

// openFavs shows the favorites-only list. Leaving it returns to the folder it
// was opened from, or to the top screen with the pseudo-entry highlighted.
func (m *model) openFavs() {
	if m.mode == modeBrowse {
		m.favBack = favEntryPath
		if it, ok := m.selected(); ok && m.cur != "" {
			m.favBack = it.path
		}
	}
	m.mode = modeFavs
	m.loadFavs(0)
}

func (m *model) loadFavs(sel int) {
	m.items = nil
	for _, f := range m.cfg.Favorites {
		m.items = append(m.items, m.favItem(f))
	}
	m.sel, m.top = min(sel, max(len(m.items)-1, 0)), 0
}

func (m *model) favsKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	n := len(m.items)
	it, ok := m.selected()
	switch key := k.String(); key {
	case "q":
		return m.quit(1)
	case "esc", "left", "F", "tab":
		m.mode = modeBrowse
		m.load(m.favBack)
	case "up", "k":
		if n > 0 {
			m.sel = (m.sel - 1 + n) % n
		}
	case "down", "j":
		if n > 0 {
			m.sel = (m.sel + 1) % n
		}
	case "enter":
		if ok {
			return m.open(it, true)
		}
	case "right":
		if ok {
			return m.open(it, false)
		}
	case "f":
		if ok {
			m.toggleFav(it.path)
			m.loadFavs(m.sel)
		}
	case "u":
		if ok {
			m.openAccounts(it.path)
		}
	default:
		if i, ok := m.pick(key); ok {
			return m.open(m.items[i], false)
		}
	}
	return nil
}

// ---- search ----------------------------------------------------------------

type batchMsg struct {
	gen     int
	entries []Entry
	done    bool
}

// waitBatch collects walk batches for up to 50ms so ranking runs in chunks.
func waitBatch(gen int, ch <-chan []Entry) tea.Cmd {
	return func() tea.Msg {
		out, ok := <-ch
		if !ok {
			return batchMsg{gen: gen, done: true}
		}
		t := time.NewTimer(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case b, ok := <-ch:
				if !ok {
					return batchMsg{gen, out, true}
				}
				out = append(out, b...)
			case <-t.C:
				return batchMsg{gen, out, false}
			}
		}
	}
}

func (m *model) textInput(prompt, value, placeholder string) {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.PromptStyle = dimS
	ti.PlaceholderStyle = dimS
	ti.TextStyle = lipgloss.NewStyle()
	ti.Cursor.Style = lipgloss.NewStyle()
	ti.Cursor.TextStyle = lipgloss.NewStyle()
	ti.Placeholder = placeholder
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.SetValue(value)
	ti.CursorEnd()
	ti.Focus()
	m.input = ti
}

func (m *model) stopWalk() {
	if m.cancelWalk != nil {
		m.cancelWalk()
		m.cancelWalk = nil
	}
	m.walking = false
	m.gen++
}

func (m *model) startSearch(entries []Entry, query string) tea.Cmd {
	m.stopWalk()
	m.mode = modeSearch
	m.textInput("/ ", query, "search folders")
	m.rsel, m.rtop = 0, 0
	m.entries, m.results = entries, nil
	if entries != nil {
		m.rerank()
		return nil
	}
	bases := rootBases(m.cfg)
	if m.cur != "" {
		bases = []Base{folderBase(m.cfg, m.cur)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelWalk = cancel
	m.walkCh = walk(ctx, bases, m.cfg.Search.MaxDepth, m.cfg.Search.Ignore)
	m.walking, m.walkStart = true, time.Now()
	return waitBatch(m.gen, m.walkCh)
}

func (m *model) rerank() {
	m.results = rank(m.entries, m.input.Value(), favSet(m.cfg))
	m.rsel = min(m.rsel, max(len(m.results)-1, 0))
}

func (m *model) onBatch(b batchMsg) tea.Cmd {
	if b.gen != m.gen {
		return nil
	}
	m.entries = append(m.entries, b.entries...)
	m.rerank()
	if b.done {
		m.walking = false
		m.walkTook = time.Since(m.walkStart)
		return nil
	}
	return waitBatch(m.gen, m.walkCh)
}

func (m *model) leaveSearch() {
	m.stopWalk()
	m.mode = modeBrowse
	back := ""
	if it, ok := m.selected(); ok {
		back = it.path
	}
	m.load(back)
}

func (m *model) searchKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	n := len(m.results)
	switch k.String() {
	case "esc":
		m.leaveSearch()
	case "up":
		if n > 0 {
			m.rsel = (m.rsel - 1 + n) % n
		}
	case "down":
		if n > 0 {
			m.rsel = (m.rsel + 1) % n
		}
	case "enter":
		if n > 0 {
			return m.finish(m.results[m.rsel].Path)
		}
	case "right":
		if n > 0 {
			p := m.results[m.rsel].Path
			m.stopWalk()
			m.enter(p, false)
		}
	case "ctrl+f":
		if n > 0 {
			m.toggleFav(m.results[m.rsel].Path)
			m.rerank()
		}
	case "ctrl+p":
		if n > 0 {
			p := m.results[m.rsel].Path
			m.stopWalk()
			return m.startSync(p)
		}
	case "ctrl+o":
		if n > 0 {
			m.openAccounts(m.results[m.rsel].Path)
		}
	default:
		old := m.input.Value()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		if m.input.Value() != old {
			m.rsel, m.rtop = 0, 0
			m.rerank()
		}
		return cmd
	}
	return nil
}

// ---- new folder ------------------------------------------------------------

func (m *model) startNew() {
	m.mode = modeNew
	m.newErr = ""
	m.textInput("", "", "folder name")
}

func validateNewFolder(parent, name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("name is empty")
	case strings.Contains(name, "/"):
		return errors.New("name must not contain /")
	case strings.TrimSpace(name) != name:
		return errors.New("name must not start or end with a space")
	case strings.ContainsAny(name[:1], ".@#"):
		return errors.New("name must not start with . @ or # (it would be hidden)")
	}
	if _, err := os.Lstat(filepath.Join(parent, name)); err == nil {
		return fmt.Errorf("%s already exists", name)
	}
	return nil
}

func (m *model) newKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.mode = modeBrowse
	case "enter", "tab":
		name := m.input.Value()
		if err := validateNewFolder(m.cur, name); err != nil {
			m.newErr = err.Error()
			return nil
		}
		p := filepath.Join(m.cur, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			m.newErr = err.Error()
			return nil
		}
		if k.String() == "enter" {
			return m.finish(p)
		}
		m.enter(p, true)
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		m.newErr = ""
		return cmd
	}
	return nil
}

// ---- view ------------------------------------------------------------------

// rows is how many list rows fit: the terminal height minus the chrome every
// list view has (blank, header, ↑/↓ markers, message, blank, hint) and the
// extra lines of the current view. At least 1.
func (m *model) rows(n, extra int) int {
	h := m.height
	if h == 0 {
		h = 24
	}
	return min(n, max(h-7-extra-(len(m.hintLines())-1), 1))
}

// list renders n rows with "↑ N more" / "↓ N more" markers, adjusting *top.
func (m *model) list(b *strings.Builder, n, sel int, top *int, extra int, row func(i, slot int, selected bool) string) {
	rows := m.rows(n, extra)
	if sel < *top {
		*top = sel
	}
	if sel > *top+rows-1 {
		*top = sel - rows + 1
	}
	*top = max(0, min(*top, n-rows))
	if *top > 0 {
		fmt.Fprintf(b, "    %s\n", dimS.Render(fmt.Sprintf("↑ %d more", *top)))
	}
	for i := *top; i < *top+rows; i++ {
		b.WriteString(row(i, i-*top, i == sel) + "\n")
	}
	if rest := n - *top - rows; rest > 0 {
		fmt.Fprintf(b, "    %s\n", dimS.Render(fmt.Sprintf("↓ %d more", rest)))
	}
}

func (m *model) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	switch m.mode {
	case modeBrowse, modeFavs:
		m.viewBrowse(&b)
	case modeSearch:
		m.viewSearch(&b)
	case modeNew:
		m.viewNew(&b)
	case modeSettings:
		m.viewSettings(&b)
	case modeSync:
		m.viewSync(&b)
	case modeAccount:
		m.viewAccount(&b)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m *model) header() string {
	if m.cur == "" {
		return dimS.Render("clauder")
	}
	return fg(m.nav.color).Render(sanitize(m.nav.label, false)) + sanitize(strings.TrimPrefix(m.cur, m.nav.base), false)
}

func (m *model) footer(b *strings.Builder) {
	if m.msg != "" {
		b.WriteString("  " + m.msg + "\n")
	}
	b.WriteString("\n")
	for _, l := range m.hintLines() {
		b.WriteString("  " + dimS.Render(l) + "\n")
	}
}

// hint is the key hint for the current view.
func (m *model) hint() string {
	switch m.mode {
	case modeFavs:
		return "↑↓ move · ⏎ run · → open · f remove" + m.acctHint(" · u account") + " · esc back · q quit"
	case modeBrowse:
		if m.cur == "" {
			return "↑↓ move · ⏎ run · → open · / find · f fav · F favs · p pull" + m.acctHint(" · u account") + " · s settings · q quit"
		}
		return "↑↓ move · ⏎ run · → open · ← up · . here · / find · n new · f fav · F favs · p pull" + m.acctHint(" · u account") + " · q quit"
	case modeSearch:
		return "↑↓ move · ⏎ run · → open · ^f fav · ^p pull" + m.acctHint(" · ^o account") + " · esc back"
	case modeAccount:
		return "↑↓ move · ⏎ run once · s set for folder · c clear (inherit) · esc back"
	case modeNew:
		return "⏎ create & run · tab create & open · esc cancel"
	case modeSettings:
		return m.settingsHint()
	case modeSync:
		if m.sync.detail {
			return "esc back"
		}
		return "↑↓ move · ⏎ output · c clone missing · esc back · q quit"
	}
	return ""
}

// hintLines wraps the hint at the terminal width on " · " boundaries.
func (m *model) hintLines() []string {
	width := m.width
	if width == 0 {
		width = 80
	}
	return wrapHint(m.hint(), width-2) // 2 = indent
}

func wrapHint(hint string, width int) []string {
	var lines []string
	cur := ""
	for _, part := range strings.Split(hint, " · ") {
		next := part
		if cur != "" {
			next = cur + " · " + part
		}
		if cur != "" && lipgloss.Width(next) > width {
			lines = append(lines, cur)
			next = part
		}
		cur = next
	}
	return append(lines, cur)
}

func (m *model) viewBrowse(b *strings.Builder) {
	top := m.cur == "" && m.mode == modeBrowse
	if m.mode == modeFavs {
		b.WriteString("  " + starS.Render("★") + " favorites\n")
	} else {
		b.WriteString("  " + m.header() + "\n")
	}
	m.list(b, len(m.items), m.sel, &m.top, m.acctExtra(), func(i, slot int, selected bool) string {
		it := m.items[i]
		key := "  "
		if slot < 9 {
			key = strconv.Itoa(slot+1) + ")"
		}
		color := it.color
		if !top && m.mode == modeBrowse {
			color = 0
		}
		label := fg(color)
		cur := fg(m.nav.color)
		if top || m.mode == modeFavs {
			cur = fg(it.color)
		}
		if it.missing {
			label = dimS
		}
		if selected {
			label = label.Bold(true)
		}
		if it.favEntry {
			label, cur = favEntryS, favEntryS
			if selected {
				label = label.Bold(true)
			}
		}
		s := label.Render(sanitize(it.label, false))
		if it.missing {
			s += " " + dimS.Render("(missing)")
		}
		if it.kids {
			s += " " + dimS.Render("›")
		}
		if it.fav {
			s += " " + starS.Render("★")
		}
		s += m.acctTag(it.path)
		if selected {
			return "  " + cur.Render("❯") + " " + dimS.Render(key) + " " + s
		}
		return "    " + dimS.Render(key) + " " + s
	})
	switch {
	case len(m.items) > 0:
	case m.mode == modeFavs:
		b.WriteString("    " + dimS.Render("(no favorites yet — press f on a folder)") + "\n")
	case m.cur == "":
		b.WriteString("    " + dimS.Render("(no roots — press s to add some)") + "\n")
	default:
		b.WriteString("    " + dimS.Render("(no subfolders)") + "\n")
	}
	target := m.cur
	if it, ok := m.selected(); ok {
		target = it.path
		if it.favEntry {
			target = ""
		}
	}
	b.WriteString(m.accountLine(target))
	m.footer(b)
}

func (m *model) viewSearch(b *strings.Builder) {
	scope := "all roots"
	if m.cur != "" {
		scope = sanitize(m.cfg.label(m.cur), false)
	}
	b.WriteString("  " + m.input.View() + "  " + dimS.Render("in "+scope) + "\n")
	status := fmt.Sprintf("%d of %d folders", len(m.results), len(m.entries))
	if m.walking {
		status += " · searching…"
	}
	b.WriteString("  " + dimS.Render(status) + "\n")
	m.list(b, len(m.results), m.rsel, &m.rtop, 1+m.acctExtra(), func(i, _ int, selected bool) string {
		r := m.results[i]
		s := highlight(r, selected)
		if r.Fav {
			s = starS.Render("★") + " " + s
		}
		s += m.acctTag(r.Path)
		if selected {
			return "  " + fg(r.Color).Render("❯") + " " + s
		}
		return "    " + s
	})
	if len(m.results) == 0 && !m.walking {
		b.WriteString("    " + dimS.Render("(no matches)") + "\n")
	}
	if len(m.results) > 0 {
		b.WriteString(m.accountLine(m.results[m.rsel].Path))
	}
	m.footer(b)
}

// highlight colours the root part of the display path and marks matched runes.
func highlight(r Result, selected bool) string {
	matched := make(map[int]bool, len(r.Idx))
	for _, i := range r.Idx {
		matched[i] = true
	}
	var b strings.Builder
	for i, ch := range r.Display {
		st := lipgloss.NewStyle()
		if i < r.Prefix {
			st = fg(r.Color)
		}
		if selected {
			st = st.Bold(true)
		}
		if matched[i] {
			st = st.Underline(true).Bold(true)
		}
		b.WriteString(st.Render(sanitize(string(ch), false)))
	}
	return b.String()
}

func (m *model) viewNew(b *strings.Builder) {
	b.WriteString("  " + m.header() + "\n")
	b.WriteString("  " + dimS.Render("new folder:") + " " + m.input.View() + "\n")
	if m.newErr != "" {
		b.WriteString("  " + errS.Render(sanitize(m.newErr, false)) + "\n")
	}
	m.footer(b)
}
