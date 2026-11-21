# Script commands

[Back to README](../README.md)

Drop a file into an app's `commands/` folder and it becomes a command. It runs
through the code tool of the server, or the host binding (see [Native hosts](native-hosts.md)),
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
{
  "exec": { "tool": "call_api", "lang": "python",
          "args": { "api_path": "exec", "args": ["python-console", ["$code"]] },
          "outputJq": "join(\"\")", "errorPrefix": "Error: ", "errorJq": "." }
}
```

The leading comment block is the command's help, and `summary:` is its one-line
summary. `server: NAME` picks the server when several run the same language. Files are
re-read on every run, so edits apply immediately. Names that are bash keywords (`select`,
`time`, ...) are hidden by bash; the listing says so, and `host NAME` runs them.

## Command folders

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
