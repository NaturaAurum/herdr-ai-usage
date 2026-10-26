# herdr-ai-usage

A [herdr](https://herdr.dev) plugin that opens a right-side panel showing how
much of your AI plan limits is left:

| Service | Default login read from | Link another account (`a`) |
|---|---|---|
| Claude | keychain (Claude Code) | `claude auth login` in its own `CLAUDE_CONFIG_DIR` |
| Codex | `~/.codex/auth.json` | `codex login` in its own `CODEX_HOME` |
| OpenCode Go | `~/.local/share/opencode/auth.json` | `opencode auth login` in its own `XDG_DATA_HOME` |
| Cursor | Cursor.app state DB | paste the `WorkosCursorSessionToken` cookie |
| Grok | `~/.grok/auth.json` | `grok login` in its own `GROK_HOME` |

Services without a login are hidden until you sign in or link one. A linked account gets its own login dir under
`herdr plugin config-dir ai-usage`, so it also works as
`CLAUDE_CONFIG_DIR=<dir> claude` or `CODEX_HOME=<dir> codex`.

If the [gh-view](https://github.com/NaturaAurum/herdr-gh-view) panel is open
in the tab, this panel stacks under it and shares the right sidebar.
Results are cached on disk and shared by every panel (Claude: 5 min,
Codex: 1 min), and `Retry-After` is honored on HTTP 429.
An expired Claude login, including Claude Code's own, is refreshed and
written back to the keychain, so the panel keeps working when Claude Code
hasn't run for a while.

Accounts are grouped by service. Each service has its own color; override
it in `$(herdr plugin config-dir ai-usage)/colors.json` with hex or an ANSI
number, then reopen the panel:

```json
{"claude": "#ff8800", "codex": "6"}
```

## Requirements

- herdr 0.9.0+, macOS (Claude login is read from the keychain)
- Go, to build on install

## Install

```bash
herdr plugin install NaturaAurum/herdr-ai-usage
herdr plugin action invoke ai-usage.open
```

```toml
[[keys.command]]
key = "prefix+u"
type = "plugin_action"
command = "ai-usage.open"
```

## Keys

`j`/`k` select · `a` add account · `d` delete linked account · `r` refresh · `q` close

## Development

```bash
go build -o herdr-ai-usage . && herdr plugin link .
```
