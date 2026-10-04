# agent-shell

Give an agent a **sandboxed bash shell over a running program** (Godot, Blender, Roblox
Studio, ...), served over MCP. The agent gets three tools, `run`, `list_commands` and
`help`, and composes everything else with pipes, loops and in-process text tools instead
of loading every tool of every server into its context.

This repo is the **Go module**. Each app is its own small binary built on it, with its
config embedded:

| App | Repo | Talks to |
|---|---|---|
| Blender | [blender-shell](https://github.com/brohd11/blender-shell) | `uvx blender-mcp` + Python script commands |
| Roblox Studio | [roblox-shell](https://github.com/brohd11/roblox-shell) | Studio's built-in MCP server + Luau editing commands |
| Godot | [godot-shell](https://github.com/brohd11/godot-shell) | the editor_console bridge (native host) |

An app's **config** decides what the shell talks to:

```
agent ──MCP──> blender-shell
                ├─ blender-mcp <tool> [--flags]   an existing MCP server, its tools as subcommands
                ├─ other-mcp <tool> ...           any number of upstream servers
                ├─ <host commands>                optional native host (the Godot bridge), top level
                ├─ objects, pick, ...             your script commands, run through a server's code tool
                └─ grep jq sort head ...          builtins, in-process
```

```sh
blender-shell run 'objects MESH | jq -r .name | pick'
blender-shell run 'blender-mcp get_object_info Cube | jq .location'
godot-shell   run 'tree root | tree nodes --recursive | grep -c Camera'
```

## Building an app

An app is a module with a `config.json`, an optional `commands/` folder, and a `main.go`:

```go
package main

import (
	"embed"

	"github.com/brohd11/agent-shell"
)

// Directory embeds skip files starting with "_": list command libraries explicitly.
//
//go:embed config.json commands commands/_lib.*
var appFS embed.FS

var version = "dev" // -ldflags "-X main.version=..."

func main() {
	agentshell.Main(agentshell.Config{
		Name:       "roblox-shell",          // binary and MCP server name
		App:        "roblox",                // config folder: ~/.agent-shell/roblox/
		Version:    version,
		UpdateRepo: "brohd11/roblox-shell",  // enables `update` via GitHub releases
		FS:         appFS,
		// Builtins: Go-side commands, which win over every other command source.
	})
}
```

Test that the embedded config loads with
`profile.Load(profile.App{Name: "roblox", FS: appFS}, profile.Options{...})`, and check
`l.Sources(...)`. App repos carry their makefile, installers and release workflow, rendered
from [sh-templates](https://github.com/brohd11/sh-templates).

For local development, put the apps next to this repo with a `go.work` that `use`s all of
them, so they build against the local core.

Every binary has the same CLI:

```sh
blender-shell setup              # user config + `claude mcp add -s user blender-shell -- <path>`
blender-shell commands           # check what the shell can reach
blender-shell run SCRIPT         # one run, "-" reads the script from stdin
blender-shell                    # the MCP server over stdio
```

`setup` creates `~/.agent-shell/blender/` (config overrides and `commands/`),
registers the server with Claude Code (`--scope`, `--name`, or `--print` to only print the
command), and lists what the app itself needs. Each app is its own MCP server
registration, so each one can be turned on and off independently.

An app starts its **own** copy of each upstream server, so it coexists with the same
server registered directly in Claude Code. Keep both, or unregister the direct one to save
context. blender-mcp's addon accepts several clients at once.

## Config

The built-in config is embedded in the app's binary. Layers apply in order, later
layers winning:

| Layer | Config | Script commands |
|---|---|---|
| built-in | embedded `config.json` | embedded `commands/` |
| user | `~/.agent-shell/<app>/config.json` | `~/.agent-shell/<app>/commands/` |
| project | `./.agent-shell/<app>.json` | `./.agent-shell/<app>/commands/` |

`AGENT_SHELL_CONFIG_DIR` moves the user folder (default `~/.agent-shell`).

The format is `.mcp.json`'s `mcpServers` plus agent-shell keys:

```json
{
  "title": "the running Blender session",
  "instructions": "Guidance for the agent...",
  "setup": "What the app needs, printed by `setup`",
  "host": { "address": "127.0.0.1:${EDITOR_CONSOLE_PORT:-9510}", "token": "${EDITOR_CONSOLE_TOKEN}", "hint": "..." },
  "mcpServers": {
    "blender-mcp": {
      "command": "uvx", "args": ["blender-mcp"], "env": {},
      "defaults": { "user_prompt": "" },
      "errorPattern": "^Error\\b",
      "hideTools": [],
      "exec": { "tool": "execute_blender_code", "param": "code", "lang": "python",
                "outputPrefix": "Code executed successfully: ", "errorPrefix": "Error executing code: " }
    },
    "remote": { "type": "http", "url": "http://localhost:8000/mcp", "headers": {} }
  },
  "root": "",
  "timeout": 120,
  "maxOutput": 65536
}
```

- `${VAR}` / `${VAR:-default}` expand in `host`, `mcpServers` and `root`, as in `.mcp.json`.
  `config show` prints the merged config *unexpanded*, so tokens from the environment are not printed.
- **Merging `mcpServers` by name:** setting any transport key (`type`, `command`, `args`, `env`,
  `url`, `headers`) replaces the transport as a unit. `exec`, `defaults`, `errorPattern`,
  `hideTools` and `disabled` override one by one. So `mcp add blender-mcp -- /my/blender-mcp`
  keeps the built-in script binding. Use `"disabled": true` to turn off a server from a lower layer.
- `defaults` fill tool arguments the agent didn't pass, when the tool declares them.
  blender-mcp marks `user_prompt` as required on most tools. A default can be
  **dynamic**: `{"$tool": "list_roblox_studios", "$jq": ".studios[0].id"}` calls that
  tool once per run and filters its JSON output. The Roblox app fills `studio_id`
  this way, and its jq `error(...)` asks for `--studio_id` when several Studios are open.
  Tool help marks parameters the config fills in.
- `errorPattern` turns "error as plain text" results into exit 1.
- `root` exposes one directory to the shell as `/`. Empty means no file access.
- Unknown keys are errors, which catches typos.

Managing servers (writes the user config, or the project config with `--project`):

```sh
godot-shell mcp add godot-mcp -- npx -y some-godot-mcp
godot-shell mcp add remote --url http://localhost:8000/mcp --header "Authorization: Bearer x"
blender-shell mcp import blender     # copy an entry from .mcp.json / ~/.claude.json
blender-shell mcp list | remove NAME
blender-shell config path | show
```

## Upstream MCP servers as commands

Each server is one command. Its tools are subcommands, and arguments follow the tool's
JSON Schema:

```sh
blender-mcp --help                          # tool list (+ the server's own instructions)
blender-mcp export_scene --help             # parameters, types, required, defaults
blender-mcp export_scene --filepath /tmp/a.glb --format glb --selection_only
blender-mcp export_scene --object_names Cube --object_names Light ...   # arrays: repeat, or JSON
blender-mcp get_object_info Cube            # bare value = the single required parameter
blender-mcp tool --json '{"a": 1}'          # whole input; --json - reads it from stdin
```

Values are converted to the schema's types (integer, number, boolean, object/array
as JSON; `anyOf` with null is handled). Unknown or missing parameters fail before
calling. Text results go to stdout; with no text, structured content is printed as
JSON. `isError` results, and results matching `errorPattern`, go to stderr with exit 1.
`-` and `_` are interchangeable in tool and parameter names.

## Script commands

Drop a file into an app's `commands/` folder and it becomes a command. It runs
through the code tool of the server whose `exec.lang` matches the file extension
(`.py` for python, `.luau` for luau), with `ARGS` (a list of strings), `STDIN` and `CWD`
defined first:

```python
# summary: select objects by name (args, or one name per stdin line)
# usage: pick NAME...        or: objects MESH | jq -r .name | pick
import bpy
names = list(ARGS) or [l.strip() for l in STDIN.splitlines() if l.strip()]
...
```

Luau scripts return what they `print` (Studio's `execute_luau` would otherwise
return only the chunk's return value). Error line numbers match the file for Luau, and
are the file's plus one for Python.

A file named `_lib.luau` (or `_lib.py`) in a commands folder is shared code rather than a
command. Every script of that language gets it, as a `lib` table in Luau. Libraries from all
layers are combined in order, so a user folder can add helpers. `exec.errorTrim` (a
regexp) strips noise from error text, such as Studio's internal `file:line:` prefixes.

The leading comment block is the command's help, and `summary:` is its one-line
summary. `server: NAME` picks the server when several run the same language. Files are
re-read on every run, so edits apply immediately. A user or project file with the same
name overrides the built-in one. Names that are bash keywords (`select`, `time`, ...)
are hidden by bash; the listing says so, and `host NAME` runs them.

## Native hosts

A program with no MCP server can speak the small host protocol directly. The program
listens on loopback TCP, and agent-shell connects once per request: one JSON line out,
one JSON line back.

```json
→ {"id":1,"method":"hello","token":"...","protocol":1}
← {"id":1,"name":"godot","version":"4.7","commands":[{"name":"tree","summary":"..."}]}
→ {"id":2,"method":"invoke","token":"...","cmd":"tree","args":["nodes","--recursive"],"stdin":"/root\n","cwd":"/"}
← {"id":2,"stdout":"...","stderr":"","exit_code":0}
→ {"id":3,"method":"help","token":"...","cmd":"tree"}      (optional)
← {"id":3,"stdout":"usage: ..."}
← {"id":N,"error":"Unauthorized"}                          (any failure)
```

- `args` is an exact argv, so the host must not re-parse it as a command line.
- Integers may arrive as floats (as Godot's JSON produces them).
- Help is never fetched by running `cmd --help`, because a command that ignored the flag would really run.
- Invokes to one host are serialized.

`hosttest/` is a complete host in about 100 lines. The Godot implementation is
`addons/editor_console/src/bridge/console_bridge.gd`; it still answers the older
`godot-editor-console-mcp` requests too.

## Sandbox and trust model

The **shell** is fixed by agent-shell, whatever the app's config:

- **No processes.** Only registered commands run. Absolute paths, `exec`, `eval`, `PATH=`
  and the like fail with 127. The interpreter's OS exec handler is never called.
- **No files by default.** Redirects, `source`, globbing, `cd`, `test -f` and builtin file
  operands are confined to `root` (via `os.Root`) or to `/dev/null`. Process substitution
  is rejected.
- **Clean environment.** jq's `$ENV` is empty.
- **Bounded.** Timeout (exit 124), output caps, stdin caps.

**Commands are as powerful as their source.** Upstream MCP servers and native hosts act
with the app's permissions: `execute_blender_code`, Roblox's `execute_luau` and the Godot
console's `expr` run arbitrary code inside the app, and Godot's `ls`/`cat` accept paths
outside the project. Upstream servers are processes agent-shell starts from your config
(never from the agent). The sandbox stops the shell from being an escape hatch; it does
not make a powerful app safe. Use `hideTools` to drop tools you don't want reachable.

## Layout

| Path | What |
|---|---|
| `agentshell.go` | CLI every app binary shares: serve, run, commands, setup, mcp, config, update |
| `profile/` | config schema, layering (app-embedded, user, project), editing, import |
| `host/` | `Source` interface; native host protocol + TCP client |
| `host/mcphost` | upstream MCP server as a namespaced command (go-sdk client) |
| `host/scripts` | script commands through a server's code tool |
| `engine/`, `engine/shengine` | engine seam; mvdan.cc/sh with sandboxed handlers |
| `builtins/` | grep head tail wc sort uniq cut tr seq sed jq cat, `help`, `host` |
| `shell/` | one run: gather sources (a down source is a warning), timeout, output caps |
| `mcpserver/` | MCP tools `run`, `list_commands`, `help` |
| `hosttest/` | in-process fake native host |

## Known limitations

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
