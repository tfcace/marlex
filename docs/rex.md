# marlex in Rex

[Rex](https://www.superlogical.com/rex) is a terminal. marlex talks to it two ways: a key that blames anything on screen, and progress reports on the tab.

## Install

```sh
mx rex install                     # adds the key cmd+shift+b and a command palette entry
mx rex install --key cmd+shift+y   # another key
mx rex install --no-key            # the command palette entry only
```

It puts `mx.lua` next to `init.lua` (`--dir` says where, if Rex reads its configuration from somewhere else) and adds one line:

```lua
require("mx").setup{ key = "cmd+shift+b" }
```

`setup` takes `key` (`false` for no key; the action `mx_why` is always in the command palette), `bin` (the command to run, `mx` by default), `open` (`"split"` for a block to the right, the default, or `"layer"` for a floating pane), `width` (the split's share of the space, 0.45) and `bounds` (the floating pane's `{x, y, w, h}` as fractions of the window).

## Blame anything on screen

The key reads the focused terminal, finds the file names and `file:line` references on it, and opens `mx why -i` for the one nearest the cursor, in a block to its right. It works on compiler errors, `go test` failures (which name files relative to their package, so marlex matches them by their trailing path), stack traces with absolute paths from any repository, grep output, diffs, and an agent's transcript. In a repository without an index, `b` in the block builds one.

In the block, ↓ digs into the line's older versions and ↑ comes back, ←/→ move between the files on screen when there are several, `s` shows the commit, `y` copies its hash and `q` closes it. A second press replaces the block instead of stacking splits.

## In an editor

In an editor, the key blames the line under the cursor and follows the cursor and the file as they change. There is only that line, no other files. marlex reads only what the editor shows, so some need a setting to show where they are:

| Program | Works out of the box | Setting |
|---|---|---|
| helix, kakoune, micro | yes, from the status line | |
| vim, neovim | the file only | `set title titlestring=%f:%l` (exact, from the terminal's title), or `set ruler` or `set number` |
| nano | the file only | `set constantshow` in `.nanorc` |
| less | the file, on its first screen | `LESS=-M`, which blames the top line shown |

Without the setting, you get the whole file's card, with a note naming the setting.

## Status on the tab

`build`, `ff`, `pull` and `publish` report what they are doing with the [Program Status Protocol](https://www.superlogical.com/rex/docs/build/program-status) (OSC 7501), so Rex shows a percentage on a tab you aren't looking at, then `done` if it took long enough for you to look away, or `error` if it failed. On a terminal, the same commands draw their progress as a framed meter. Reports are on in Rex (`TERM_PROGRAM=rex`) and anywhere with `MX_STATUS=1`, and off with `MX_STATUS=0`.
