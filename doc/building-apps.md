# Building an app

[Back to README](../README.md)

An integration embeds its config and optional script commands, then calls `mcpsh.Main`:

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
		Name:       "mcp_sh_krita", // binary and MCP server name
		App:        "krita",        // config folder: ~/.mcp-sh/krita/
		Version:    version,
		UpdateRepo: "brohd11/mcp-sh-krita", // enables `update` via GitHub releases
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

For changes spanning the library and apps, use a Go workspace; see [Development](development.md).

Every binary has the same CLI:

```sh
mcp_sh_krita setup              # user config + prints the claude/codex mcp add commands
mcp_sh_krita commands           # check what the shell can reach
mcp_sh_krita run SCRIPT         # one run, "-" reads the script from stdin
mcp_sh_krita                    # the MCP server over stdio
```

`setup` creates user config and prints registration commands for Claude Code and
Codex; run the appropriate printed command to register the server. It also prints
app-specific setup instructions. `--help` includes the same registration commands.
Use `--scope` for Claude's scope and `--name` to change the registration name.

`Config.Registrations` adds alternate entries, such as Godot's game server on port
9511. Each entry can be enabled independently in the MCP client.

Choose a [native host](native-hosts.md) or an [upstream MCP server](upstream-servers.md)
to connect the shell to a program. Add [script commands](script-commands.md) for reusable
operations and [configuration layers](configuration.md) for user and project overrides.
