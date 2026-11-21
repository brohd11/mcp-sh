# mcp-sh

A Go library for giving agents a sandboxed bash shell over a running program, served
through MCP. Agents get three tools—`run`, `list_commands` and `help`—and combine app
commands with pipes, loops and builtins such as `jq` and `grep`.

Each integration is a small binary with embedded configuration. It connects to a
bridge inside the program, wraps an existing MCP server, or both. The shell blocks
OS process execution and restricts file access; app commands retain the program's
permissions. See [Sandbox and limitations](doc/sandbox.md).

For ready-made integrations, see [Godot](https://github.com/brohd11/mcp-sh-godot),
[GIMP](https://github.com/brohd11/mcp-sh-gimp) and
[Krita](https://github.com/brohd11/mcp-sh-krita).

## Install

Requires Go 1.26.4 or newer. Add the library to a new integration module:

**macOS / Linux (sh)**

```sh
mkdir my-shell
cd my-shell
go mod init example.com/my-shell
go get github.com/brohd11/mcp-sh
```

**Windows (PowerShell)**

```powershell
mkdir my-shell
cd my-shell
go mod init example.com/my-shell
go get github.com/brohd11/mcp-sh
```

## Quickstart

This example connects to a Godot editor bridge on port 9510. Enable the bridge using
[Godot's setup instructions](https://github.com/brohd11/mcp-sh-godot#install), or use
your own native host's address. The CLI requires a host or upstream server in its config.

Create `config.json`:

```json
{
  "host": { "address": "127.0.0.1:9510" }
}
```

Create `main.go`:

```go
package main

import (
	"embed"

	"github.com/brohd11/mcp-sh"
)

//go:embed config.json
var appFS embed.FS

func main() {
	mcpsh.Main(mcpsh.Config{
		Name: "my_shell",
		App:  "my-shell",
		FS:   appFS,
	})
}
```

Run this in either shell:

```sh
go run . run 'echo hello | tr a-z A-Z'
```

It prints `HELLO`. With the bridge running, `go run . commands` lists the host's
commands too. See [Native hosts](doc/native-hosts.md) for the bridge protocol or
[Upstream MCP servers](doc/upstream-servers.md) to wrap an existing server.
[Building an app](doc/building-apps.md) covers embedding scripts and the shared CLI.

Build and prepare MCP registration:

```sh
go build -o my_shell .
./my_shell setup
```

On Windows, use `go build -o my_shell.exe .` and `.\my_shell.exe setup`.
Run the appropriate registration command printed by `setup` for your MCP client;
`setup` creates user config but does not register the server itself. Launching the
binary without arguments serves MCP over stdio.

## Documentation

- [Building an app](doc/building-apps.md): integration entry point and shared CLI.
- [Configuration](doc/configuration.md): layers, settings and server management.
- [Upstream MCP servers](doc/upstream-servers.md): tools as shell subcommands.
- [Script commands](doc/script-commands.md): reusable commands and shared libraries.
- [Native hosts](doc/native-hosts.md): bridge protocol and script execution.
- [Sandbox and limitations](doc/sandbox.md): permissions, bounds and shell differences.
- [Development](doc/development.md): tests, workspaces and source layout.
