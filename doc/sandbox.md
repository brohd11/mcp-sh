# Sandbox and limitations

[Back to README](../README.md)

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
