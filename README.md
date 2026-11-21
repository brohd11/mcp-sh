# mcp-sh

Give an agent a **sandboxed bash shell over a running program** (Godot, GIMP, Krita, ...),
served over MCP. The agent gets three tools, `run`, `list_commands` and `help`, and
composes everything else with pipes, loops and in-process text tools instead of loading
every tool of every server into its context.

This repo is the **Go module**. Each app is its own small binary built on it, with its
config embedded:

| App | Repo | Binary | Talks to |
|---|---|---|---|
| Godot | [mcp-sh-godot](https://github.com/brohd11/mcp-sh-godot) | `mcp_sh_godot` | its own editor addon, or a running game's bridge (native host) |
| GIMP | [mcp-sh-gimp](https://github.com/brohd11/mcp-sh-gimp) | `mcp_sh_gimp` | its own plug-in (native host) + Python script commands |
| Krita | [mcp-sh-krita](https://github.com/brohd11/mcp-sh-krita) | `mcp_sh_krita` | its own plug-in (native host) + Python script commands |

Repos and modules are named `mcp-sh-<host>`. The binary and its MCP server name are
`mcp_sh_<host>`, with underscores.

An app's **config** decides what the shell talks to:

```
agent ──MCP──> mcp_sh_gimp
                ├─ <host commands>                native host (the GIMP bridge), top level
                ├─ layers, filter, ...            script commands, run through the host's `python` command
                ├─ some-mcp <tool> [--flags]      optional upstream MCP servers, their tools as subcommands
                └─ grep jq sort head ...          builtins, in-process
```

```sh
mcp_sh_godot run 'tree root | tree nodes --recursive | grep -c Camera'
mcp_sh_gimp  run 'images | jq -r .image'
mcp_sh_krita run 'layers | jq -r .path | grep -c Background'
```

## Building an app

An app is a module with a `config.json`, an optional `commands/` folder, and a `main.go`:

```go
package main

import (
	"embed"

	"github.com/brohd11/mcp-sh"
)

// Directory embeds skip files starting with "_": list command libraries explicitly.
//
//go:embed config.json commands commands/_lib.py
var appFS embed.FS

var version = "dev" // -ldflags "-X main.version=..."

func main() {
	mcpsh.Main(mcpsh.Config{
		Name:       "mcp_sh_krita",          // binary and MCP server name
		App:        "krita",                 // config folder: ~/.mcp-sh/krita/
		Version:    version,
		UpdateRepo: "brohd11/mcp-sh-krita",  // enables `update` via GitHub releases
		FS:         appFS,
		// Builtins: Go-side commands, which win over every other command source.
		// Subcommands: extra CLI verbs, e.g. mcp_sh_godot's `addon install`.
		// Registrations: extra server entries, e.g. mcp_sh_godot_game on port 9511.
	})
}
```

Test that the embedded config loads with
`profile.Load(profile.App{Name: "krita", FS: appFS}, profile.Options{...})`, and check
`l.Sources(...)`. App repos carry their makefile, installers and release workflow, rendered
from [sh-templates](https://github.com/brohd11/sh-templates).

For local development, put the apps next to this repo with a `go.work` that `use`s all of
them, so they build against the local core.

Every binary has the same CLI:

```sh
mcp_sh_krita setup              # user config + prints the claude/codex mcp add commands
mcp_sh_krita commands           # check what the shell can reach
mcp_sh_krita run SCRIPT         # one run, "-" reads the script from stdin
mcp_sh_krita                    # the MCP server over stdio
```

`setup` creates `~/.mcp-sh/krita/` (config overrides and `commands/`), prints the
commands that register the server with Claude Code and Codex (`--scope` for Claude,
`--name`), and lists what the app itself needs. It registers nothing itself, and `--help`
ends with the same commands. An app can add entries of its own (`Config.Registrations`),
such as the same binary on another port: mcp_sh_godot also prints `mcp_sh_godot_game`,
for a running game's bridge on 9511. Each app is its own MCP server registration, so each
one can be turned on and off independently.

An app starts its **own** copy of each upstream server, so it coexists with the same
server registered directly in Claude Code. Keep both, or unregister the direct one to save
context, as long as the program behind the server accepts several clients at once.

## Config

The built-in config is embedded in the app's binary. Layers apply in order, later
layers winning:

| Layer | Config | Script commands |
|---|---|---|
| built-in | embedded `config.json` | embedded `commands/` |
| user | `~/.mcp-sh/<app>/config.json` | `~/.mcp-sh/<app>/commands/` |
| project | `./.mcp-sh/<app>.json` | `./.mcp-sh/<app>/commands/` |
| local | `./.mcp-sh/local/<app>.json` | `./.mcp-sh/local/<app>/commands/` |

The local layer is private to you and this project. When mcp-sh writes it, it adds a
`.gitignore` of `*` to `.mcp-sh/local/`, so servers with tokens in `env` or `headers` stay out of
git. Its commands folder is only listed once the layer is in use.

`MCP_SH_CONFIG_DIR` moves the user folder (default `~/.mcp-sh`).

The format is `.mcp.json`'s `mcpServers` plus mcp-sh keys:

```json
{
  "title": "the live Godot editor",
  "instructions": "Guidance for the agent...",
  "setup": "What the app needs, printed by `setup`",
  "host": { "address": "127.0.0.1:${MCP_SH_GODOT_PORT:-9510}", "token": "${MCP_SH_GODOT_TOKEN}", "hint": "...", "timeout": 0 },
  "mcpServers": {
    "some-mcp": {
      "command": "npx", "args": ["-y", "some-mcp"], "env": {},
      "defaults": { "user_prompt": "" },
      "errorPattern": "^Error\\b",
      "hideTools": [],
      "exec": { "tool": "execute_code", "param": "code", "lang": "python",
                "outputPrefix": "Code executed successfully: ", "errorPrefix": "Error executing code: " }
    },
    "remote": { "type": "http", "url": "http://localhost:8000/mcp", "headers": {} }
  },
  "commandDirs": ["~/code/my-commands"],
  "root": "",
  "timeout": 120,
  "maxOutput": 65536
}
```

- `${VAR}` / `${VAR:-default}` expand in `host`, `mcpServers` and `root`, as in `.mcp.json`.
  `config show` prints the merged config *unexpanded*, so tokens from the environment are not printed.
- **Merging `mcpServers` by name:** setting any transport key (`type`, `command`, `args`, `env`,
  `url`, `headers`) replaces the transport as a unit. `exec`, `defaults`, `errorPattern`,
  `hideTools` and `disabled` override one by one. So `mcp add some-mcp -- /my/some-mcp`
  keeps the built-in script binding. Use `"disabled": true` to turn off a server from a lower layer.
- `defaults` fill tool arguments the agent didn't pass, when the tool declares them, such as
  a `user_prompt` that a server marks as required on most tools. A default can be
  **dynamic**: `{"$tool": "list_sessions", "$jq": ".sessions[0].id"}` calls that
  tool once per run and filters its JSON output. That fills a `session_id` from the first
  open session, and a jq `error(...)` in the filter can ask for `--session_id` when several
  are open. Tool help marks parameters the config fills in.
- `errorPattern` turns "error as plain text" results into exit 1.
- `exec` runs script commands through the server's code tool (see [Script commands](#script-commands)).
- `root` exposes one directory to the shell as `/`. Empty means no file access.
- `host.timeout` (seconds) limits each host command on its own. The default 0 leaves only the
  script's `timeout`, since some host commands (exports, scans) legitimately take a while.
- Unknown keys are errors, which catches typos.

Managing servers (writes the user config unless `--user`, `--project` or `--local` picks a
layer):

```sh
mcp_sh_godot mcp add some-mcp -- npx -y some-mcp
mcp_sh_godot mcp add remote --url http://localhost:8000/mcp --header "Authorization: Bearer x"
mcp_sh_godot mcp import some-mcp      # copy an entry from .mcp.json / ~/.claude.json
mcp_sh_godot mcp list | remove NAME   # remove finds the layer; asks when it's in several
mcp_sh_godot config path | show
```

`import` keeps the server's Claude Code scope: a `.mcp.json` server goes to the project
config, a user server (`~/.claude.json`) to the user config, and a local one (that project's
entry in `~/.claude.json`) to the local config. A layer flag overrides this.

## Upstream MCP servers as commands

Each server is one command. Its tools are subcommands, and arguments follow the tool's
JSON Schema:

```sh
some-mcp --help                          # tool list (+ the server's own instructions)
some-mcp export_scene --help             # parameters, types, required, defaults
some-mcp export_scene --filepath /tmp/a.glb --format glb --selection_only
some-mcp export_scene --object_names Cube --object_names Light ...   # arrays: repeat, or JSON
some-mcp get_object_info Cube            # bare value = the single required parameter
some-mcp tool --json '{"a": 1}'          # whole input; --json - reads it from stdin
```

Values are converted to the schema's types (integer, number, boolean, object/array
as JSON; `anyOf` with null is handled). Unknown or missing parameters fail before
calling. Text results go to stdout; with no text, structured content is printed as
JSON. `isError` results, and results matching `errorPattern`, go to stderr with exit 1.
`-` and `_` are interchangeable in tool and parameter names.

Images in a result (a screenshot, a render) are saved to `~/.mcp-sh/<app>/images/`
and printed as their path, one per line, so the agent can open them with its own tools
(`some-mcp screenshot | tail -1`). The folder keeps the newest 50 images.

## Script commands

Drop a file into an app's `commands/` folder and it becomes a command. It runs
through the code tool of the server, or the host binding (see [Native hosts](#native-hosts)),
whose `exec.lang` matches the file extension (`.py` for python, `.luau` for luau), with
`ARGS` (a list of strings), `STDIN` and `CWD` defined first. mcp-sh-gimp's `layers`:

```python
# summary: every layer of the open images, inside groups too, as JSON lines with an id and path
# usage: layers [IMAGE...]      IMAGE: #ID or file name; default: every open image
images = [image_ref(a) for a in ARGS] or list(Gimp.get_images())
rows = [layer_info(layer, path) for img in images for layer, path in walk(img)]
for row in rows:
    emit(row)
```

Luau scripts return what they `print`, where a code tool would otherwise return only the
chunk's return value. Error line numbers match the file for Luau, and are the file's plus
one for Python.

A file named `_lib.py` (or `_lib.luau`) in a commands folder is shared code rather than a
command. Every script of that language gets it (`image_ref`, `walk` and `emit` above come
from GIMP's), as a `lib` table in Luau. Libraries from all folders are combined in order, so
your folders can add helpers. `exec.errorTrim` (a regexp) strips noise from error text, such
as a server's internal `file:line:` prefixes.

`exec.param` names the tool's code argument. For a tool that takes the code nested in
other arguments, give its whole input as `exec.args` instead, with the string `"$code"`
where the code goes. `exec.outputJq` and `exec.errorJq` are jq filters for tools that
answer in JSON; text that is not JSON is kept as it is. For example, a `call_api` tool
that takes the code in a list and returns a JSON list of printed text:

```json
"exec": { "tool": "call_api", "lang": "python",
          "args": { "api_path": "exec", "args": ["python-console", ["$code"]] },
          "outputJq": "join(\"\")", "errorPrefix": "Error: ", "errorJq": "." }
```

The leading comment block is the command's help, and `summary:` is its one-line
summary. `server: NAME` picks the server when several run the same language. Files are
re-read on every run, so edits apply immediately. Names that are bash keywords (`select`,
`time`, ...) are hidden by bash; the listing says so, and `host NAME` runs them.

### Your own command folders

Keep your own commands wherever suits you, such as a git repo, and point the app at it:

```sh
mcp_sh_krita commands add ~/code/krita-tools             # user config
mcp_sh_krita commands add ./tools/krita --project        # this project's config
mcp_sh_krita commands add ./tools/private --local        # private, gitignored
mcp_sh_krita commands remove ~/code/krita-tools
```

That edits `commandDirs` in the config. Entries expand `${VAR}` and `~/`, and a relative
entry is relative to the config file's folder. Folders are searched in this order, later
ones winning when two scripts share a name:

1. the built-in `commands/`
2. `~/.mcp-sh/<app>/commands/`, then the user config's `commandDirs`
3. `./.mcp-sh/<app>/commands/`, then the project config's `commandDirs`
4. `./.mcp-sh/local/<app>/commands/`, then the local config's `commandDirs`

A script that replaces one from an earlier folder still works, but its listing says so:
`layers  my layers (overrides the one in built-in krita commands)`. A `commandDirs` folder
that doesn't exist is reported in the `commands` source list (`missing: ...`) and in
`config path`.

## Native hosts

A program with no MCP server can speak the small host protocol directly. The program
listens on loopback TCP, and mcp-sh connects once per request: one JSON line out,
one JSON line back.

```json
→ {"id":1,"method":"hello","token":"...","protocol":1}
← {"id":1,"name":"godot","version":"4.7","commands":[{"name":"tree","summary":"..."}]}
→ {"id":2,"method":"invoke","token":"...","cmd":"tree","args":["nodes","--recursive"],"stdin":"/root\n","cwd":"/","timeout_ms":119000,"max_output":4194304}
← {"id":2,"stdout":"...","stderr":"","exit_code":0}
→ {"id":3,"method":"help","token":"...","cmd":"tree"}      (optional)
← {"id":3,"stdout":"usage: ..."}
← {"id":N,"error":"Unauthorized"}                          (any failure)
```

- `args` is an exact argv, so the host must not re-parse it as a command line.
- Integers may arrive as floats (as Godot's JSON produces them).
- Help is never fetched by running `cmd --help`, because a command that ignored the flag would really run.
- Invokes to one host are serialized.
- `timeout_ms` (time left before mcp-sh stops waiting; absent when there is no deadline) and
  `max_output` (bytes) are advisory. When mcp-sh gives up it closes the connection. A host
  that checks these, or watches for the close, can stop the command itself. One that doesn't
  keeps working, and requests queued behind it wait. The Godot bridge passes all three to gdsh,
  whose long commands check them.

`hosttest/` is a complete host in about 100 lines. The Godot implementation is
mcp-sh-godot's `addon/mcp_sh_godot/src/remote/addons/_lib/mcp_sh_bridge/src/bridge.gd`.

A host can also run script commands. Give it a command that executes code from its stdin,
and point `host.exec` at it:

```json
"host": { "address": "127.0.0.1:9520", "exec": { "command": "python", "lang": "python" } }
```

Each script command is then sent to that host command, wrapped as usual (`ARGS`, `STDIN`,
`CWD`, the `_lib`), and the host's stdout, stderr and exit code are passed through as they
are. That keeps what a script printed before it failed, and needs none of the MCP binding's
prefix or jq options. mcp-sh-gimp's bridge (`plugin/mcp-sh-gimp-bridge/mcp-sh-gimp-bridge.py`)
and mcp-sh-krita's (`plugin/mcp_sh_krita_bridge/`) work this way. A script's `server:` line names a host binding as `host`.

## Sandbox and trust model

The **shell** is fixed by mcp-sh, whatever the app's config:

- **No processes.** Only registered commands run. Absolute paths, `exec`, `eval`, `PATH=`
  and the like fail with 127. The interpreter's OS exec handler is never called.
- **No files by default.** Redirects, `source`, globbing, `cd`, `test -f` and builtin file
  operands are confined to `root` (via `os.Root`) or to `/dev/null`. Process substitution
  is rejected. Without `root`, `cd` and `pwd` go to the host's commands of those names if
  it has them, so a host keeps its own working directory (Godot's `res://`).
- **Clean environment.** jq's `$ENV` is empty.
- **Bounded.** Timeout (exit 124), output caps, stdin caps.

**Commands are as powerful as their source.** Upstream MCP servers and native hosts act
with the app's permissions: GIMP's and Krita's `python` and the Godot console's `expr` run
arbitrary code inside the app, and Godot's `ls`/`cat` accept paths
outside the project. Upstream servers are processes mcp-sh starts from your config
(never from the agent). The sandbox stops the shell from being an escape hatch; it does
not make a powerful app safe. Use `hideTools` to drop tools you don't want reachable.

## Layout

| Path | What |
|---|---|
| `mcpsh.go` | CLI every app binary shares: serve, run, commands, setup, mcp, config, update |
| `profile/` | config schema, layering (app-embedded, user, project, local), editing, import |
| `host/` | `Source` interface; native host protocol + TCP client |
| `host/mcphost` | upstream MCP server as a namespaced command (go-sdk client) |
| `host/scripts` | script commands through a server's code tool |
| `engine/`, `engine/shengine` | engine seam; mvdan.cc/sh with sandboxed handlers |
| `builtins/` | grep head tail wc sort uniq cut tr seq sed jq cat, `help`, `host` |
| `shell/` | one run: gather sources (a down source is a warning), timeout, output caps |
| `mcpserver/` | MCP tools `run`, `list_commands`, `help` |
| `hosttest/` | in-process fake native host |

## Known limitations

- No `PIPESTATUS`: the embedded interpreter (mvdan/sh) doesn't implement it. A pipeline's
  status is its last command's; `set -o pipefail` makes a failure anywhere count, e.g.
  `set -o pipefail; layers | layer set visible false | head -1 || echo failed`.
- A pure-shell loop piped into `head` runs until it ends or times out, because the
  interpreter has no SIGPIPE. Go builtins and command output stop on EPIPE.
- Builtins cover the common flags only, and fail loudly on others. `sed` only supports `s///`.
- Script commands can't set an exit code beyond 0/1. An exception, or output matching
  `errorPrefix`, is 1.
- No elicitation or sampling passthrough: upstream servers that ask the client questions
  get no answer.

## Development

```sh
go test ./...
go test -race ./...
```
