//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// palette is used for roots without an explicit colour (256-colour codes).
var palette = []int{39, 141, 214, 71, 203, 44, 177, 149, 209, 111}

type Root struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Color int    `json:"color,omitempty"` // 0 = auto from palette
}

type SearchConfig struct {
	MaxDepth int      `json:"max_depth"`
	Ignore   []string `json:"ignore"`
}

type GitConfig struct {
	Jobs    int      `json:"jobs"`
	Timeout string   `json:"timeout"`
	Exclude []string `json:"exclude"`
}

// Config holds paths expanded in memory; Save writes them back with "~".
type Config struct {
	Roots     []Root       `json:"roots"`
	Favorites []string     `json:"favorites"`
	Command   string       `json:"command"`
	Search    SearchConfig `json:"search"`
	Git       GitConfig    `json:"git"`

	file  string
	extra map[string]json.RawMessage // unknown top-level keys, kept on save
}

var configKeys = []string{"roots", "favorites", "command", "search", "git"}

var defaultIgnore = func() []string {
	ig := []string{".git", "node_modules", "@eaDir", "#recycle", ".terraform", ".venv"}
	if runtime.GOOS == "darwin" {
		ig = append(ig, "Library")
	}
	return ig
}()

func defaultConfig() *Config {
	return &Config{
		Roots:     []Root{{Name: "home", Path: "~"}},
		Favorites: []string{},
	}
}

// configPath resolves: --config > $CLAUDER_CONFIG > $XDG_CONFIG_HOME > ~/.config.
func configPath(flag string) (string, error) {
	if flag != "" {
		return expandPath(flag), nil
	}
	if p := os.Getenv("CLAUDER_CONFIG"); p != "" {
		return expandPath(p), nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "clauder", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "clauder", "config.json"), nil
}

// loadConfig reads path, creating a default config when it does not exist.
func loadConfig(path string) (cfg *Config, created bool, err error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = defaultConfig()
		created = true
	case err != nil:
		return nil, false, err
	default:
		cfg = &Config{}
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		if err := json.Unmarshal(data, &cfg.extra); err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		for _, k := range configKeys {
			delete(cfg.extra, k)
		}
	}
	cfg.file = path
	cfg.fillDefaults()
	if _, err := time.ParseDuration(cfg.Git.Timeout); err != nil {
		return nil, false, fmt.Errorf("%s: git.timeout: %w", path, err)
	}
	for i := range cfg.Roots {
		cfg.Roots[i].Path = expandPath(cfg.Roots[i].Path)
	}
	for i, f := range cfg.Favorites {
		cfg.Favorites[i] = expandPath(f)
	}
	if created {
		if err := cfg.Save(); err != nil {
			return nil, false, err
		}
	}
	return cfg, created, nil
}

func (c *Config) fillDefaults() {
	if c.Command == "" {
		c.Command = "claude"
	}
	if c.Favorites == nil {
		c.Favorites = []string{}
	}
	if c.Search.MaxDepth <= 0 {
		c.Search.MaxDepth = 4
	}
	if c.Search.Ignore == nil {
		c.Search.Ignore = slices.Clone(defaultIgnore)
	}
	if c.Git.Jobs <= 0 {
		c.Git.Jobs = 8
	}
	if c.Git.Timeout == "" {
		c.Git.Timeout = "120s"
	}
	if c.Git.Exclude == nil {
		c.Git.Exclude = []string{}
	}
}

func (c *Config) GitTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Git.Timeout)
	return d
}

// Save writes the config atomically (temp file + fsync + rename) as indented
// JSON. A symlinked config is written through the link; the file mode and
// unknown top-level keys are kept.
func (c *Config) Save() error {
	data, err := c.marshal()
	if err != nil {
		return err
	}
	target := c.file
	if real, err := filepath.EvalSymlinks(c.file); err == nil {
		target = real
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// marshal renders the known keys in a fixed order, then unknown keys sorted.
func (c *Config) marshal() ([]byte, error) {
	roots := slices.Clone(c.Roots)
	for i := range roots {
		roots[i].Path = tildePath(roots[i].Path)
	}
	favs := make([]string, len(c.Favorites))
	for i, f := range c.Favorites {
		favs[i] = tildePath(f)
	}
	var b bytes.Buffer
	b.WriteString("{")
	write := func(k string, raw []byte) error {
		if b.Len() > 1 {
			b.WriteString(",")
		}
		key, _ := json.Marshal(k)
		b.WriteString("\n  ")
		b.Write(key)
		b.WriteString(": ")
		return json.Indent(&b, raw, "  ", "  ")
	}
	for i, v := range []any{roots, favs, c.Command, c.Search, c.Git} {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if err := write(configKeys[i], raw); err != nil {
			return nil, err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(c.extra)) {
		if err := write(k, c.extra[k]); err != nil {
			return nil, err
		}
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// RootColor returns the root's colour, auto-assigned by position when unset.
func (c *Config) RootColor(i int) int {
	if c.Roots[i].Color != 0 {
		return c.Roots[i].Color
	}
	return palette[i%len(palette)]
}

// nextFreeColor picks the first palette colour no root uses yet.
func (c *Config) nextFreeColor() int {
	used := map[int]bool{}
	for i := range c.Roots {
		used[c.RootColor(i)] = true
	}
	for _, p := range palette {
		if !used[p] {
			return p
		}
	}
	return palette[len(c.Roots)%len(palette)]
}

// locate returns the index of the deepest root containing p and the path
// relative to it ("" when p is the root itself), or -1.
func (c *Config) locate(p string) (int, string) {
	best, rel := -1, ""
	for i, r := range c.Roots {
		if r.Path == p {
			if best < 0 || len(r.Path) > len(c.Roots[best].Path) {
				best, rel = i, ""
			}
			continue
		}
		prefix := strings.TrimSuffix(r.Path, "/") + "/"
		if strings.HasPrefix(p, prefix) && (best < 0 || len(r.Path) > len(c.Roots[best].Path)) {
			best, rel = i, strings.TrimPrefix(p, prefix)
		}
	}
	return best, rel
}

// label is "root/rel" for paths under a root, else a ~/... path.
func (c *Config) label(p string) string {
	i, rel := c.locate(p)
	switch {
	case i < 0:
		return tildePath(p)
	case rel == "":
		return c.Roots[i].Name
	default:
		return c.Roots[i].Name + "/" + rel
	}
}

func (c *Config) IsFavorite(p string) bool { return slices.Contains(c.Favorites, p) }

// ToggleFavorite adds or removes p and saves. It reports whether p is now a favorite.
func (c *Config) ToggleFavorite(p string) (bool, error) {
	if i := slices.Index(c.Favorites, p); i >= 0 {
		c.Favorites = slices.Delete(c.Favorites, i, i+1)
		return false, c.Save()
	}
	c.Favorites = append(c.Favorites, p)
	return true, c.Save()
}

func (c *Config) rootIndex(path string) int {
	return slices.IndexFunc(c.Roots, func(r Root) bool { return r.Path == path })
}

func (c *Config) nameTaken(name string, except int) bool {
	for i, r := range c.Roots {
		if i != except && strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

// uniqueName returns base, or base-2, base-3, … if taken.
func (c *Config) uniqueName(base string) string {
	name := base
	for n := 2; c.nameTaken(name, -1); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

// AddRoot appends a root with an auto-assigned colour and saves.
func (c *Config) AddRoot(name, path string) error {
	c.Roots = append(c.Roots, Root{Name: name, Path: path, Color: c.nextFreeColor()})
	return c.Save()
}

func validRootName(c *Config, name string, except int) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("name is empty")
	case strings.Contains(name, "/"):
		return errors.New("name must not contain /")
	case c.nameTaken(name, except):
		return fmt.Errorf("a root named %q already exists", name)
	}
	return nil
}

func defaultRootName(path string) string {
	b := strings.ToLower(filepath.Base(path))
	if b == "/" || b == "." || b == "" {
		return "root"
	}
	return b
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// expandPath expands a leading ~ and cleans the path.
func expandPath(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	if p == "" {
		return p
	}
	return filepath.Clean(p)
}

// tildePath replaces the $HOME prefix with ~.
func tildePath(p string) string {
	h := homeDir()
	if h == "" || h == "/" {
		return p
	}
	if p == h {
		return "~"
	}
	if strings.HasPrefix(p, h+"/") {
		return "~/" + p[len(h)+1:]
	}
	return p
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
