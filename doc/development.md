# Development

[Back to README](../README.md)

```sh
go test ./...
go test -race ./...
```

For changes spanning multiple modules, clone the apps beside this repo and include
them in a `go.work` file. This builds against the local library instead of its published tag.

## Source layout

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
