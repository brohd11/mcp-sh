# Configuration

[Back to README](../README.md)

Config layers apply in this order, with later values winning:

| Layer | Config | Script commands |
|---|---|---|
| built-in | embedded `config.json` | embedded `commands/` |
| user | `~/.mcp-sh/<app>/config.json` | `~/.mcp-sh/<app>/commands/` |
| project | `./.mcp-sh/<app>.json` | `./.mcp-sh/<app>/commands/` |
| local | `./.mcp-sh/local/<app>.json` | `./.mcp-sh/local/<app>/commands/` |

The local layer is private to this project. When writing it, mcp-sh adds a `.gitignore`
containing `*` to `.mcp-sh/local/`. Its commands folder is listed only when the layer is in use.

`MCP_SH_CONFIG_DIR` moves the user folder (default `~/.mcp-sh`).

The format extends `.mcp.json`'s `mcpServers` with shell and host settings:

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
- `defaults` fill omitted arguments declared by a tool's schema. A dynamic default such
  as `{"$tool": "list_sessions", "$jq": ".sessions[0].id"}` calls the tool once per run
  and filters its JSON. Use jq's `error(...)` to require an explicit argument when the
  result is ambiguous. Tool help marks parameters supplied by config.
- `errorPattern` turns "error as plain text" results into exit 1.
- `exec` runs script commands through the server's code tool (see [Script commands](script-commands.md)).
- `root` exposes one directory to the shell as `/`. Empty means no file access.
- `host.timeout` (seconds) limits each host command on its own. The default 0 leaves only the
  script's `timeout`, since some host commands (exports, scans) legitimately take a while.
- Unknown keys are errors, which catches typos.

## Managing servers

These commands write user config by default; `--project` and `--local` select another layer:

```sh
mcp_sh_godot mcp add some-mcp -- npx -y some-mcp
mcp_sh_godot mcp add remote --url http://localhost:8000/mcp --header "Authorization: Bearer x"
mcp_sh_godot mcp import some-mcp      # copy an entry from .mcp.json / ~/.claude.json
mcp_sh_godot mcp list
mcp_sh_godot mcp remove NAME         # requires a layer flag if defined in several layers
mcp_sh_godot config path
mcp_sh_godot config show
```

`import` keeps the server's Claude Code scope: a `.mcp.json` server goes to the project
config, a user server (`~/.claude.json`) to the user config, and a local one (that project's
entry in `~/.claude.json`) to the local config. A layer flag overrides this.
