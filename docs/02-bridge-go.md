# 02 — `ai-contextview mcp` (Go bridge)

The bridge is a stdio MCP server launched by Claude Code, one per Claude
instance. It is thin and stateless apart from its connection: identity,
validation, symbol resolution and forwarding.

## Module layout

The bridge and the viewer daemon (03) are one Go module and one binary,
`ai-contextview`; the bridge is the `mcp` subcommand.

```
go.mod                             # module github.com/rrgmc/ai-contextview
cmd/ai-contextview/main.go               # subcommands: mcp, serve, fakeapp, version
internal/
├── identity/      # workspace id, cwd, claude pid discovery
├── appconn/       # socket client: dial, start daemon, hello, reconnect, request/response
├── tools/         # MCP tool handlers (spec_show, focus, ask, ask_wait)
├── validate/      # input rules from 01-protocol.md
├── specsrc/       # markdown source: headings, artifacts, quote matching (shared with daemon)
├── resolve/       # code refs → ResolvedRef (go/packages, lines)
├── fakeapp/       # terminal test double for the daemon (`ai-contextview fakeapp`)
└── protocol/      # envelope + message structs (shared with daemon, tested against /protocol/fixtures)
```

Dependencies:
- `github.com/modelcontextprotocol/go-sdk`: MCP server and stdio transport
- `golang.org/x/tools/go/packages`: symbol resolution
- `github.com/yuin/goldmark`: parsing markdown headings in `specsrc`
- standard library for everything else

## Startup sequence

1. `identity`:
   - `ws` = new UUIDv4.
   - `cwd` = `os.Getwd()` (Claude starts the server in the project dir).
   - `claude_pid`: walk up from `os.Getppid()` until a process named `claude`
     (or `node` running Claude Code) is found. Fall back to the plain ppid. It is
     informational only (shown in the tab tooltip), so it doesn't need to be
     exact.
   - `project_name` = the base name of the git root of cwd, else of cwd.
2. Start the MCP server on stdio **immediately**. Don't block on the daemon,
   because Claude Code will time out the server start.
3. `appconn` connects lazily, on the first tool call, in the background:
   - Dial the socket path (see 01). On `ENOENT`/`ECONNREFUSED`, start the
     daemon: run its own executable as `ai-contextview serve --detach` (or
     `$AI_CONTEXTVIEW_DAEMON` when set, e.g. `ai-contextview fakeapp` during M1). Then
     retry with backoff (100ms → 2s, total 10s).
   - Send `hello`, wait for `hello_ack`, and keep `url` (the workspace page).
4. If the daemon can't be reached, tool calls return an MCP **tool error**:
   *"ai-contextview viewer not reachable; ask your question in the chat instead."* The
   session keeps working without the UI.

## Tool handlers

### `spec_show`
1. Resolve the path against cwd; check it exists and is either a
   `.md`/`.markdown` file or a directory containing markdown files.
2. `specsrc.LoadRoot(path, primary)` → primary file, its headings, and (for a
   folder) the artifact list with inferred kinds (01).
3. Remember it as `currentRoot` (used to resolve doc ref paths, for quote
   pre-checks, and for replay after reconnect).
4. Forward `spec_show`, wait for `result`, and return it. The result also
   includes `url`, so Claude can tell the user where the viewer is. Unless
   `AI_CONTEXTVIEW_OPEN` is `browser` or `never`, it includes `open: "pane"`,
   and the skill opens `url` in the desktop app's browser pane.

### `focus`
Validate → fold `spec_quote` into `doc_refs` → pre-check and resolve doc refs →
resolve code refs → forward → return the doc matches, `spec_match` and the
resolved code refs.

### `ask`
1. `validate.Ask(input, refsOptional)`: counts, required refs (optional in ask
   mode), string lengths.
2. **Quote pre-check** (`specsrc`): fold `spec_quote`/`spec_section` into
   `doc_refs[0]`, resolve each doc ref's path against `currentRoot` (no path =
   the primary file), then for each, normalize the file's source and the quote
   and check for a match. This is the same `specsrc.Match` the daemon uses to
   highlight, so a passing pre-check means the highlight will land.
   Normalization:
   - Unicode NFC, lowercase.
   - Strip markdown inline syntax: `*`, `_`, `` ` ``, `~~`, link syntax
     `[text](url)` → `text`, image syntax, HTML tags.
   - Collapse all whitespace (including newlines and list bullets) to a single
     space.
   - Map typographic quotes and dashes to ASCII.

   If there's no `exact`/`normalized`/`fuzzy` match, return a tool error naming
   the file and listing the 3 headings whose sections have the best token
   overlap with the quote. This catches Claude
   paraphrasing before anything reaches the UI.
3. Resolve every code ref (below). Collect *all* failures into one error
   message, so Claude can fix them in one retry.
4. Create a `qs_id`, forward `ask`, and wait for `ask_shown`. If any match is
   `none`, send `ask_cancel` and return a tool error. If `viewers` is 0, open
   the workspace `url` with `open` (macOS), `xdg-open` (Linux) or
   `rundll32 url.dll,FileProtocolHandler` (Windows), unless
   `AI_CONTEXTVIEW_OPEN=never`.
5. Wait for `ask_result` up to `AI_CONTEXTVIEW_ASK_WAIT` (default **10 min**):
   - Got `ask_result` → map it to the tool result (01).
   - Timed out → return `status: "pending"` with `qs_id`. **The question stays
     shown in the viewer.**
   - The MCP request is cancelled (the user pressed Esc in Claude) → send
     `ask_cancel{reason:"client_cancelled"}`.

   - `ask_shown` has `in_chat` (the questions are in the chat, `ask_here`)
     → return `status: "ask_in_chat"` at once.
   - Nobody used the page by the nudge delay (01, Failure modes) → send
     `ask_here {on, auto}` and return `ask_in_chat`, unless
     `AI_CONTEXTVIEW_AWAY=wait`.

### `ask_here`
Send `ask_here {on: !off}` and return `{in_chat, closed?, message}` (01).

### `ask_wait`
Wait again on the stored `qs_id`. If the daemon already sent `ask_result` while
nobody was waiting, the bridge has buffered it, so return it right away.

### Timeouts
- Claude Code applies an MCP tool timeout (`MCP_TOOL_TIMEOUT`, in ms). The
  install docs (04) set it to e.g. `1800000`. `AI_CONTEXTVIEW_ASK_WAIT` must stay
  **below** it, so the bridge always answers first with `pending` instead of
  Claude seeing a timeout error.

## Code ref resolution (`resolve`)

```go
type Resolver interface {
    Resolve(ctx context.Context, cwd string, ref CodeRef) (ResolvedRef, error)
}
```

- **Doc refs** are resolved in the same pass: path → absolute, must be markdown
  inside `currentRoot` (or cwd for a single-file root), kind taken from the
  artifact list.
- **`lines`**: parse `N` or `N-M`, clamp to the file length, and require the
  file to exist. Paths are relative to cwd; reject paths outside the git root
  unless they're absolute and exist.
- **`symbol`** (`.go` files):
  1. Find the module root for `path` (walk up looking for `go.mod`).
  2. Load the package containing `path`:
     `packages.Load(&packages.Config{Mode: NeedName|NeedFiles|NeedSyntax|NeedTypes|NeedTypesInfo, Dir: moduleRoot}, "file="+abs)`.
  3. Parse the symbol syntax: `pkg.Func`, `pkg.Type`, `pkg.(*Type).Method`,
     `pkg.Type.Method`, `pkg.Var`. The package qualifier may be the package
     name *or* omitted.
  4. Look the name up in `pkg.Types.Scope()`. For methods, use
     `types.NewMethodSet` / `LookupFieldOrMethod`.
  5. Find the declaration node in the syntax tree (`FuncDecl`, `TypeSpec`,
     `ValueSpec`) and turn its `Pos()`/`End()` into lines. Include the doc
     comment in the range when present.
  6. If the symbol is not found: return the closest names (Levenshtein ≤ 3 over
     the scope names) in the error.
- **Cache**: one `packages.Load` result per package directory, invalidated when
  any of its files' mtimes change. Loading takes 100ms to seconds; the cache
  keeps repeated questions fast.
- A symbol on a non-`.go` file is an error ("symbols are only supported for
  Go; use lines").

## Reconnect behaviour

`appconn` keeps the last `spec_show` and the pending `ask`. On EOF:
- Reconnect in the background with backoff, starting the daemon again if needed.
- Re-send `hello` (same `ws`), then `spec_show`, then the pending `ask`.
- If the daemon comes back before the pending wait ends, the blocking `ask` call
  just continues.

## Shutdown

stdin EOF (Claude exited) → send `bye`, close the socket, exit 0. Also handle
SIGTERM/SIGINT the same way.

## `ai-contextview fakeapp`

A subcommand that listens on the same socket and prints every message. For
`ask`, it prompts for answers on the terminal. This lets the whole MCP path be
tested with real Claude Code before the web viewer exists (milestone M1), and it
is the in-process fake used by the `tools` tests.

Like `serve`, it takes the daemon lock (`<socket>.lock`) before it touches the
socket. While a daemon runs, `fakeapp` exits with an error and leaves the
socket alone; otherwise it would remove the daemon's socket file and cut the
daemon off from its bridges. It takes no arguments: `-h` or `--help` prints
its usage and exits, and any other argument is an error.

## Tests

- **Golden fixtures**: marshal/unmarshal every message against
  `protocol/fixtures/*.json`.
- **`specsrc`**: table tests for normalization (bold, code spans, links, line
  breaks inside the quote, smart quotes), fuzzy acceptance/rejection, artifact
  kind inference and primary-file selection for folders.
- **`resolve`**: a testdata module with funcs, methods (pointer and value
  receivers), generic types, types and vars; check the line ranges and the
  suggestions for near-misses.
- **`tools`**: handler tests against an in-process fake daemon connection,
  covering timeouts (`pending`), cancellation, and buffered results for
  `ask_wait`.
- **End-to-end**: run `ai-contextview mcp` under the go-sdk test client with
  `ai-contextview fakeapp`, and later with a real `ai-contextview serve` driven over HTTP.
