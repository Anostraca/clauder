# clauder

Pick a project folder in a small terminal menu and run [Claude Code](https://docs.claude.com/en/docs/claude-code)
(`claude`) there. You can also fuzzy-find folders, keep favorites, create folders, and `git pull` whole
groups of repos.

Runs on macOS and Linux (amd64 and arm64). Claude Code must be installed, with `claude` on your `PATH`.

Not affiliated with Anthropic.

## Install

Homebrew (macOS and Linux):

```sh
brew install Anostraca/tap/clauder
```

With Go 1.27 or newer:

```sh
go install github.com/Anostraca/clauder@latest
```

From source:

```sh
git clone https://github.com/Anostraca/clauder && cd clauder
make install        # builds and installs to ~/.local/bin/clauder
make test
make dist           # cross-builds darwin/linux × arm64/amd64 into dist/
make clean          # removes ./clauder and dist/
```

Release binaries are on the [releases page](https://github.com/Anostraca/clauder/releases).

## First run

The first run creates `~/.config/clauder/config.json` with one root (`home` = `~`) and
opens **settings**. Press `a` to add your project folders, then `esc`.

## Config

Location: `--config PATH`, else `$CLAUDER_CONFIG`, else `$XDG_CONFIG_HOME/clauder/config.json`,
else `~/.config/clauder/config.json`. Edit it with `clauder --edit-config` or from the settings view.

```json
{
  "roots": [
    { "name": "work", "path": "~/work", "color": 39 },
    { "name": "oss",  "path": "~/src/oss" }
  ],
  "favorites": ["~/work/api"],
  "command": "claude",
  "search": { "max_depth": 4, "ignore": [".git", "node_modules", "@eaDir", "#recycle", ".terraform", ".venv"] },
  "git": { "jobs": 8, "timeout": "120s", "exclude": ["archive"] },
  "accounts": [
    { "name": "personal", "config_dir": "~/.claude" },
    { "name": "work", "config_dir": "~/.claude-work" }
  ],
  "default_account": "personal",
  "folder_accounts": { "~/work": "work" }
}
```

- `color` is a 256-colour code; leave it out to get one from the palette.
- clauder saves the file itself (favorites, settings). Saves are atomic, follow a symlinked
  config, keep the file mode, and keep top-level keys clauder doesn't know.
- Paths may use `~`. A root whose folder is missing shows as "(missing)".
- `search.max_depth` is counted from where the search starts. Names in `search.ignore`, dotdirs,
  and names starting with `@` or `#` are skipped. On macOS the default ignore list also has `Library`.
- `git.exclude` holds globs matched against repo paths relative to the sync target (`**` works).
- `accounts`, `default_account` and `folder_accounts` are optional; see [Accounts](#accounts).
  They are left out of the file while empty.

## Accounts

Use several Claude subscriptions (for example a personal Max plan and a company Team seat) side by side.
Claude Code keeps one login per config directory (`CLAUDE_CONFIG_DIR`), so each clauder account is a name
plus a config dir:

- An account whose dir is `~/.claude` (or empty) uses your normal Claude login: clauder runs claude with
  `CLAUDE_CONFIG_DIR` removed from the environment. Any other dir is passed as `CLAUDE_CONFIG_DIR`.
- The account for a run is: the folder's own account, else the account of the nearest parent folder that has
  one (assignments are inherited by subfolders, a deeper one wins), else `default_account`. With no accounts
  configured clauder leaves the environment alone.
- The first run with a new account has claude ask you to log in. clauder shows each account's email and
  plan (e.g. `Team 5x`, `Max 20x`) from Claude's own config; it never reads credentials.
- Adding an account with a new dir in settings creates the dir (mode 0700) and symlinks your shared setup
  from `~/.claude` into it: `CLAUDE.md`, `settings.json`, `keybindings.json`, `agents`, `commands`, `hooks`,
  `output-styles`, `plugins`, `rules`, `skills`. Only items that exist in `~/.claude` and are not in the new
  dir yet are linked; nothing is replaced. Login, `.claude.json`, history, projects and sessions stay per account.
- Deleting an account removes it and its folder assignments from the config; its dir stays on disk.

## Keys

**Browse.** The top screen shows your roots. When you have favorites, a `★ favorites ›` entry comes
first and opens the favorites list. Inside a root you see its subfolders, read live from disk. A dim `›`
means the folder has subfolders, and a `★` after a name marks a favorite.

| Key | Action |
|---|---|
| `↑↓` / `j k` | move (wraps around) |
| `⏎` | run claude in the highlighted folder |
| `c` | run `claude --continue` there: pick up the folder's last conversation (in an empty folder: the current folder) |
| `→` | open it (if it has no subfolders, run claude right away) |
| `1`–`9` | same as `→` on that visible row |
| `←` / `-` / `⌫` | go up |
| `.` | run claude in the current folder |
| `/` | fuzzy search (all roots on the top screen, otherwise the current subtree) |
| `n` | new folder here: `⏎` creates it and runs claude, `tab` creates it and opens it (no `/`, no leading/trailing spaces, no leading `.` `@` `#`) |
| `f` | toggle the highlighted folder as a favorite |
| `F` / `tab` | favorites list: `⏎` runs, `c` continues, `→` opens in browse, `f` removes, `←`/`esc` go back |
| `+` | add the highlighted folder as a root |
| `p` | git sync the highlighted folder (see below) |
| `u` | account selector for the highlighted folder (when accounts are configured) |
| `s` | settings (top screen) |
| `q` / `esc` | cancel (exit code 1); `ctrl-c` exits with 130 |

**Search.** Type to filter. Results rank favorites first, then fuzzy score, then shorter path.
`⏎` runs the highlighted match, `ctrl-r` runs it with `--continue`, `→` opens it in browse mode, `ctrl-f` toggles favorite,
`ctrl-p` syncs it, `ctrl-o` opens the account selector for it, `esc` goes back.

**Accounts.** With accounts configured, a dim line under the list shows which account `⏎` would use for the
highlighted folder, and folders with an account of their own carry a dim `[name]` tag. `u` (or `ctrl-o` in
search) opens the selector: it lists each account with email, plan and markers for the global default and
the folder's assignment. `⏎` runs claude there once with the selected account, `s` makes it the folder's
account (inherited by subfolders), `c` clears the folder's own account so it inherits again, `esc` goes back.

**Settings.** `tab` switches between the roots and accounts panes. Roots: `a` add (path with `tab`
completion, then name), `r` rename, `d`/`x` delete (asks y/n), `c` cycle colour, `J`/`K` or `shift-↑↓`
reorder. Accounts: `a` add (name, then config dir with `tab` completion; `~/.claude` is suggested for the
first account, `~/.claude-<name>` after that), `r` rename, `d`/`x` delete (asks y/n, keeps the dir),
`⏎` make it the global default (the first account added starts as default). `esc`/`q` back. Every change
is saved immediately.

## Git sync

`p` in the menu, or `clauder --pull [PATH]`, runs `git pull --ff-only` on every repo of a
folder in parallel. Pulls are fast-forward only, so they never create merge commits. A dirty
working tree is flagged, not skipped.

Repos are found by walking the folder (up to 4 levels). Each directory is handled on its own:
1. A [meta](https://github.com/mateodelnorte/meta) `.meta` file (`{"projects": {"dir": "clone-url"}}`)
   adds its projects. Project paths must stay inside the `.meta` directory. A bad entry or an unreadable
   `.meta` shows up as a failed row.
2. Otherwise a directory with `.git` is a plain repo, and the walk doesn't go inside it.
3. Otherwise the walk continues into its subfolders.

Repos that hold `.meta` files (the folder itself if it is a repo, and meta repos) are pulled first. Then
the `.meta` files are read again, so projects added upstream show up in the same run. Repos listed in
`.meta` that aren't on disk are reported as missing. Press `c` in the sync view, or pass `--clone-missing`,
to clone them.

Git never prompts (`GIT_TERMINAL_PROMPT=0`, ssh `BatchMode=yes`). Unless you set `GIT_SSH`, `GIT_SSH_COMMAND`
or `core.sshCommand`, ssh connections are shared. clauder opens one master connection per ssh host before
the parallel jobs start ("connecting to host…") and closes it at the end. Agents such as 1Password then
sign once per host instead of once per repo. A failure that looks like a transient ssh-agent error is
retried once at the end. Ctrl-C stops all jobs and removes half-finished clones.

In the sync view: `⏎` shows a repo's git output and `esc` goes back. Symbols: `✓` updated, `=` up to date,
`✗` failed, `∅` missing, `⚠` dirty.

## Flags

clauder's flags are long-only. At `--`, or at the first argument clauder doesn't recognise,
everything from there on is passed to claude unchanged (`clauder --resume`, `clauder -c`, `clauder "fix the bug"`).

| Flag | |
|---|---|
| `--find QUERY` | fuzzy-find across roots: one match acts directly, several open the search view, none exits 1 |
| `--print-path` | print the chosen folder to stdout instead of running claude: `cd "$(clauder --print-path)"` |
| `--favs` | start in the favorites view |
| `--settings` | start in settings |
| `--account NAME` | use this account for this run instead of the folder's (with `--find` and TUI picks; `--print-path` ignores it); an unknown name exits 2 |
| `--pull [PATH]` | sync repos under PATH (default: current dir); exit 1 if any failed |
| `--clone-missing` | with `--pull`: clone missing `.meta` repos |
| `--jobs N` | with `--pull`: parallel git jobs (default `git.jobs`) |
| `--config PATH` | use another config file |
| `--edit-config` | open the config in `$EDITOR` (default `vi`) |
| `--help`, `--version` | |

`NO_COLOR` is respected.

## License

MIT, see [LICENSE](LICENSE).
