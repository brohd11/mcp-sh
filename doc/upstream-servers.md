# Upstream MCP servers

[Back to README](../README.md)

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

Each app starts its own copy of each upstream server. It can coexist with a direct MCP
registration if the underlying program accepts multiple clients. Configure transports,
default arguments and script bindings in [Configuration](configuration.md).
