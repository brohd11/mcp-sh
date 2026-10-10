# mcp-sh

Give an agent a **sandboxed bash shell over a running program** (Godot, Blender, Roblox
Studio, ...), served over MCP. The agent gets three tools, `run`, `list_commands` and
`help`, and composes everything else with pipes, loops and in-process text tools instead
of loading every tool of every server into its context.

This repo is the **Go module**. Each app is its own small binary built on it, with its
config embedded:

| App | Repo | Talks to |
|---|---|---|
| Blender | [mcp-sh-blender](https://github.com/brohd11/mcp-sh-blender) | `uvx blender-mcp` + Python script commands |
| Roblox Studio | [mcp-sh-roblox](https://github.com/brohd11/mcp-sh-roblox) | Studio's built-in MCP server + Luau editing commands |
| GIMP | [mcp-sh-gimp](https://github.com/brohd11/mcp-sh-gimp) | its own plug-in (native host) + Python script commands |
| Krita | [mcp-sh-krita](https://github.com/brohd11/mcp-sh-krita) | its own plug-in (native host) + Python script commands |
| Godot | [mcp-sh-godot](https://github.com/brohd11/mcp-sh-godot) | its own editor addon (native host) |

An app's **config** decides what the shell talks to:

```
agent ──MCP──> mcp-sh-blender
                ├─ blender-mcp <tool> [--flags]   an existing MCP server, its tools as subcommands
                ├─ other-mcp <tool> ...           any number of upstream servers
                ├─ <host commands>                optional native host (the Godot bridge), top level
                ├─ objects, pick, ...             your script commands, run through a server's code tool
                └─ grep jq sort head ...          builtins, in-process
```

```sh
mcp-sh-blender run 'objects MESH | jq -r .name | pick'
mcp-sh-blender run 'blender-mcp get_object_info Cube | jq .location'
mcp-sh-godot   run 'tree root | tree nodes --recursive | grep -c Camera'
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
//go:embed config.json commands commands/_lib.*
var appFS embed.FS

var version = "dev" // -ldflags "-X main.version=..."

func main() {
	mcpsh.Main(mcpsh.Config{
		Name:       "mcp-sh-roblox",          // binary and MCP server name
		App:        "roblox",                // config folder: ~/.mcp-sh/roblox/
		Version:    version,
		UpdateRepo: "brohd11/mcp-sh-roblox",  // enables `update` via GitHub releases
		FS:         appFS,
		// Builtins: Go-side commands, which win over every other command source.
		// Subcommands: extra CLI verbs, e.g. mcp-sh-godot's `addon install`.
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
mcp-sh-blender setup              # user config + `claude mcp add -s user mcp-sh-blender -- <path>`
mcp-sh-blender commands           # check what the shell can reach
mcp-sh-blender run SCRIPT         # one run, "-" reads the script from stdin
mcp-sh-blender                    # the MCP server over stdio
```

`setup` creates `~/.mcp-sh/blender/` (config overrides and `commands/`),
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
| user | `~/.mcp-sh/<app>/config.json` | `~/.mcp-sh/<app>/commands/` |
| project | `./.mcp-sh/<app>.json` | `./.mcp-sh/<app>/commands/` |

`MCP_SH_CONFIG_DIR` moves the user folder (default `~/.mcp-sh`).

The format is `.mcp.json`'s `mcpServers` plus mcp-sh keys:

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
  `hideTools` and `disabled` override one by one. So `mcp add blender-mcp -- /my/blender-mcp`
  keeps the built-in script binding. Use `"disabled": true` to turn off a server from a lower layer.
- `defaults` fill tool arguments the agent didn't pass, when the tool declares them.
  blender-mcp marks `user_prompt` as required on most tools. A default can be
  **dynamic**: `{"$tool": "list_roblox_studios", "$jq": ".studios[0].id"}` calls that
  tool once per run and filters its JSON output. The Roblox app fills `studio_id`
  this way, and its jq `error(...)` asks for `--studio_id` when several Studios are open.
  Tool help marks parameters the config fills in.
- `errorPattern` turns "error as plain text" results into exit 1.
- `exec` runs script commands through the server's code tool (see [Script commands](#script-commands)).
- `root` exposes one directory to the shell as `/`. Empty means no file access.
- Unknown keys are errors, which catches typos.

Managing servers (writes the user config, or the project config with `--project`):

```sh
mcp-sh-godot mcp add godot-mcp -- npx -y some-godot-mcp
mcp-sh-godot mcp add remote --url http://localhost:8000/mcp --header "Authorization: Bearer x"
mcp-sh-blender mcp import blender     # copy an entry from .mcp.json / ~/.claude.json
mcp-sh-blender mcp list | remove NAME
mcp-sh-blender config path | show
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

Images in a result (a screenshot, a render) are saved to `~/.mcp-sh/<app>/images/`
and printed as their path, one per line, so the agent can open them with its own tools
(`blender-mcp get_viewport_screenshot | tail -1`). The folder keeps the newest 50 images.

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
folders are combined in order, so your folders can add helpers. `exec.errorTrim` (a
regexp) strips noise from error text, such as Studio's internal `file:line:` prefixes.

`exec.param` names the tool's code argument. For a tool that takes the code nested in
other arguments, give its whole input as `exec.args` instead, with the string `"$code"`
where the code goes. `exec.outputJq` and `exec.errorJq` are jq filters for tools that
answer in JSON; text that is not JSON is kept as it is. gimp-mcp's `call_api`, for
example, takes the code in a list and returns a JSON list of printed text:

```json
"exec": { "tool": "call_api", "lang": "python",
          "args": { "api_path": "exec", "args": ["pyGObject-console", ["$code"]] },
          "outputJq": "join(\"\")", "errorPrefix": "Error: ", "errorJq": "." }
```

The leading comment block is the command's help, and `summary:` is its one-line
summary. `server: NAME` picks the server when several run the same language. Files are
re-read on every run, so edits apply immediately. Names that are bash keywords (`select`,
`time`, ...) are hidden by bash; the listing says so, and `host NAME` runs them.

### Your own command folders

Keep your own commands wherever suits you, such as a git repo, and point the app at it:

```sh
mcp-sh-roblox commands add ~/code/roblox-tools             # user config
mcp-sh-roblox commands add ./tools/roblox --project        # this project's config
mcp-sh-roblox commands remove ~/code/roblox-tools
```

That edits `commandDirs` in the config. Entries expand `${VAR}` and `~/`, and a relative
entry is relative to the config file's folder. Folders are searched in this order, later
ones winning when two scripts share a name:

1. the built-in `commands/`
2. `~/.mcp-sh/<app>/commands/`, then the user config's `commandDirs`
3. `./.mcp-sh/<app>/commands/`, then the project config's `commandDirs`

A script that replaces one from an earlier folder still works, but its listing says so:
`query  my query (overrides the one in built-in roblox commands)`. A `commandDirs` folder
that doesn't exist is reported in the `commands` source list (`missing: ...`) and in
`config path`.

## Native hosts

A program with no MCP server can speak the small host protocol directly. The program
listens on loopback TCP, and mcp-sh connects once per request: one JSON line out,
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
mcp-sh-godot's `addon/mcp_sh_godot/bridge.gd`.

A host can also run script commands. Give it a command that executes code from its stdin,
and point `host.exec` at it:

```json
"host": { "address": "127.0.0.1:9520", "exec": { "command": "python", "lang": "python" } }
```

Each script command is then sent to that host command, wrapped as usual (`ARGS`, `STDIN`,
`CWD`, the `_lib`), and the host's stdout, stderr and exit code are passed through as they
are. That keeps what a script printed before it failed, and needs none of the MCP binding's
prefix or jq options. mcp-sh-gimp's bridge (`plugin/mcp-sh-gimp-bridge/mcp-sh-gimp-bridge.py`)
works this way. A script's `server:` line names a host binding as `host`.

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
with the app's permissions: `execute_blender_code`, Roblox's `execute_luau` and the Godot
console's `expr` run arbitrary code inside the app, and Godot's `ls`/`cat` accept paths
outside the project. Upstream servers are processes mcp-sh starts from your config
(never from the agent). The sandbox stops the shell from being an escape hatch; it does
not make a powerful app safe. Use `hideTools` to drop tools you don't want reachable.

## Layout

| Path | What |
|---|---|
| `mcpsh.go` | CLI every app binary shares: serve, run, commands, setup, mcp, config, update |
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
