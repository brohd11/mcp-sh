# Native hosts

[Back to README](../README.md)

A program with no MCP server can speak the small host protocol directly. The program
listens on loopback TCP, and mcp-sh connects once per request: one JSON line out,
one JSON line back.

```text
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

[`hosttest/`](../hosttest/) provides a test host. See also the
[Godot bridge](https://github.com/brohd11/godot-mcp-sh-bridge).

## Script execution

A host can also run [script commands](script-commands.md). Give it a command that executes code from its stdin,
and point `host.exec` at it:

```json
{
  "host": {
    "address": "127.0.0.1:9520",
    "exec": { "command": "python", "lang": "python" }
  }
}
```

Each script command is then sent to that host command, wrapped as usual (`ARGS`, `STDIN`,
`CWD`, the `_lib`), and the host's stdout, stderr and exit code are passed through as they
are. That keeps what a script printed before it failed, and needs none of the MCP binding's
prefix or jq options. mcp-sh-gimp's bridge (`plugin/mcp-sh-gimp-bridge/mcp-sh-gimp-bridge.py`)
and mcp-sh-krita's (`plugin/mcp_sh_krita_bridge/`) work this way. A script's `server:` line names a host binding as `host`.
