# 03 — `ai-contextview serve` (Go web viewer)

A long-lived Go daemon. It speaks the socket protocol (01) to bridges and serves
a small web UI on `127.0.0.1`. It knows nothing about MCP.

## Stack

- Go standard library: `net`, `net/http` (1.22+ routing patterns),
  `html/template`, `embed`.
- **goldmark** (+ GFM extension) for markdown → HTML, with an AST transformer
  for quote highlighting (below). Already a bridge dependency.
- **chroma v2** for code → HTML: line numbers, line anchors, `HighlightLines`.
- **sergi/go-diff** (diff-match-patch port) for fuzzy quote matching.
- **fsnotify** for file watching.
- Front end: **htmx** + its SSE extension, plus ~200 lines of vanilla JS for
  scrolling, tabs and keyboard shortcuts. All files embedded with `go:embed`.
  No npm, no bundler.

## Source layout

Single Go module, shared with the bridge:

```
cmd/ai-contextview/main.go          # subcommands: mcp, serve, fakeapp, version
internal/
├── protocol/                 # envelope + message structs (shared with bridge)
├── specsrc/                  # normalize + quote matching on markdown source (shared)
├── daemon/
│   ├── sockserver.go         # Unix socket listener, one Conn per bridge
│   ├── conn.go               # NDJSON framing, dispatch, ws binding
│   ├── workspaces.go         # ws id → *Workspace; lifecycle, adoption
│   ├── workspace.go          # root, artifacts, code refs, pending QS, history
│   ├── watch.go              # fsnotify wrapper (re-add on atomic save, debounce)
│   └── hub.go                # SSE subscribers per workspace, fan-out
├── render/
│   ├── markdown.go           # goldmark setup, section index, change hashes
│   ├── highlight.go          # AST transformer: wrap quote range in <mark>
│   └── code.go               # chroma rendering with highlighted lines
└── web/
    ├── server.go             # routes, Host/Origin checks
    ├── handlers.go
    ├── templates/*.html
    └── static/{app.js, app.css, htmx.min.js, htmx-sse.js}
```

## Process and lifecycle

- `ai-contextview serve` runs in the foreground (useful while developing).
  `ai-contextview serve --detach` forks into the background and exits once the socket
  is listening; the bridge uses this to start it.
- **Single instance**: on start, try to connect to the socket. If that works,
  another daemon is running: print its URL and exit 0. Otherwise take an
  exclusive lock (`flock`, `LockFileEx` on Windows) on `<socket>.lock`; if another daemon holds it, exit
  0. Only the lock holder removes a stale socket file and listens, so several
  bridges starting a daemon at the same moment get one daemon. The kernel drops
  the lock when the process ends, even on `kill -9`.
- `--restart` asks the running daemon to exit (a `shutdown` message on the
  socket, only accepted from the same uid), then starts.
- HTTP listens on `127.0.0.1:7878` by default (`--addr` / `$AI_CONTEXTVIEW_ADDR`). If
  the port is taken, try the next 9 ports. The chosen URL is returned to bridges
  in `hello_ack.url`.
- **Idle exit** (optional, default off): exit after 30 min with no bridges and no
  browser pages.

## Local security

The daemon binds to loopback only, but any local web page can still try to talk
to it. Keep it cheap and strict:
- Reject requests whose `Host` isn't `127.0.0.1:<port>` or `localhost:<port>`
  (blocks DNS rebinding).
- For POSTs, require `Origin` to be the daemon's own origin (blocks CSRF from
  other sites).
- No CORS headers. Nothing is ever served to other origins.

## Workspace lifecycle

States: `live` → `archived` → (closed by user).

- **`hello`**: create a workspace labelled `project_name`, with cwd, Claude PID
  and start time shown in its header.
  - **Adoption**: if an *archived* workspace has the same `cwd` and was archived
    less than 2h ago, adopt it: move its history, spec root and code refs to the
    new `ws`, and report `hello_ack.adopted_from`. Open pages for the old `ws`
    get an SSE `redirect` event to the new URL. This covers `claude --resume` and
    bridge/daemon restarts.
- **Disconnect / `bye`**: mark the workspace archived. Pages grey out and show
  "session ended". A pending question set shows as "Claude disconnected".
- Keep at most 10 archived workspaces, dropping the oldest. "Close" on the index
  page removes one.

**Isolation**: every socket message is routed by its connection's `ws`; every
HTTP route is under `/w/{ws}`. Workspaces share nothing except watched-file
entries (refcounted), which only read.

## HTTP routes

| Route | What |
|---|---|
| `GET /` | Index: live workspaces (pending-question badge), then archived ones |
| `GET /w/{ws}` | The workspace page (full HTML) |
| `GET /w/{ws}/events` | SSE stream for that workspace; `c` is the page's id, `h=1` when it starts hidden |
| `GET /w/{ws}/doc?path=…` | Rendered artifact fragment (for switching docs) |
| `GET /w/{ws}/code?path=…&start=…&end=…` | Rendered code fragment |
| `POST /w/{ws}/qs/{qs}/answer` | Form: answers, other, notes → `ask_result{answered}` |
| `POST /w/{ws}/qs/{qs}/in-chat` | → `ask_result{in_chat}` |
| `POST /w/{ws}/qs/{qs}/dismiss` | → `ask_result{dismissed}` |
| `POST /w/{ws}/close` | Close an archived workspace |
| `POST /w/{ws}/presence` | Form: `c` (the page's id), `hidden` (`1` or `0`, when the page is hidden or shown), `active` (`1` when someone used the page: mouse, keyboard, scroll; sent at most every 5 seconds; it also moves questions that were in the chat back to the viewer, `ask_here`) → 204 |
| `POST /w/{ws}/open` | Form: `kind` (`doc` with a document key, or `code` with a code tab's absolute path), `line`, `editor` → opens the file in the editor; other files are refused |
| `GET /static/…` | Embedded assets |

The first answer POST for a `qs` wins. Later POSTs (another tab) get `409` and
the page shows "already answered".

### SSE events

Each event carries an HTML fragment that htmx swaps in by target id
(`hx-swap-oob`), or a small JSON payload handled by `app.js`.

| event | payload | effect |
|---|---|---|
| `doc` | rendered artifact HTML + changed-section ids | Replace the doc pane, keep scroll position (below) |
| `artifacts` | artifact list HTML | Replace the artifact sidebar (folders only) |
| `focus` | `{doc: {path, mark_id}, code: [{path, start, end}]}` | Switch doc/code tabs, scroll marks into view |
| `ask` | question panel HTML | Show the panel, auto-focus Q1, notify |
| `ask_closed` | `{qs_id, status}` | Collapse the panel to a summary, append to history |
| `state` | `{live: bool}` | Grey out on disconnect |
| `redirect` | `{url}` | Navigate (after adoption) |

On reconnect the browser's `EventSource` sends `Last-Event-ID`; the daemon
ignores it and simply re-sends the current state (doc, focus, pending ask).
State is small, so no event log is needed.

## Page layout

```
┌ ai-contextview · shop · retry.md ● ──────────────────────── [Questions below] ◐ ┐
├──────────────┬───────────────────────────┬────────────────────────────────┤
│ Artifacts    │ Doc pane                  │ Question panel                 │
│ (folders)    │ ## Retry policy ◆changed  │ Retry semantics   ‹ Q 1/2 ›    │
│ ▸ proposal   │ …▓quoted text▓…           │ Should retries reset the       │
│ ▸ adr/002    │                           │ backoff on partial success?    │
│ ▸ research/… │                           │ [§ Retry policy] [retry.go:41] │
│              │                           │ (•) Reset  Recommended         │
│              │                           │ ( ) Keep growing               │
│              │                           │ [Next ›] [Send answers]        │
│              │                           │ [Back to chat] [Skip]          │
│              │                           ├────────────────────────────────┤
│              │                           │ Code pane                      │
│              │                           │ [retry.go] [client.go]         │
│              │                           │ 41 func (r *Retrier) Next() {  │
│              │                           │ 42 ▓  d := r.base << r.n▓      │
└──────────────┴───────────────────────────┴────────────────────────────────┘
```

- The spec takes the left half of the space beside the artifact sidebar by
  default. The right column holds the
  question panel above the code pane. "Questions below" in the header switches
  to a layout with the panel docked under both panes; "Questions beside"
  switches back.
- **Questions in chat**: a header pill shown while the session's questions go
  to the chat because nobody was at the page (`ask_here`, 01). Using the page
  hides it and brings the questions back. The history lists each set asked
  in the chat as "Asked in chat".
- CSS grid; the artifact sidebar only appears when the root is a folder.
- **Layout per question set** (07): the doc pane shows while a spec is shown,
  and for a whole question set when any of its questions has a document.
  Otherwise (`no-doc`) the question panel and the code pane share the page,
  side by side, or stacked with "Questions below". The layout changes only
  when a new question set arrives, so it does not jump between questions.
- **No code to show** (`no-code`): when the current question has no code ref,
  or no file is shown, the code pane and its splitter hide and the question
  panel takes the whole right column. This only applies with the questions
  above the code: there the pane hides below the panel, so nothing moves
  sideways. In the other layouts the code pane sits beside another pane and
  stays, with its "no code reference" note.
- **Narrow window** (760px or less): the panes stack in one column that the
  page scrolls, with the question panel first. Each pane has a fixed height
  and scrolls inside. The splitters and "Questions below" hide.
- Pane sizes and the layout choice are remembered in `localStorage` (wrapped in
  try/catch, per-browser convenience only).
- The panel is part of the page, never a modal or a `<dialog>`, so the doc and
  code stay scrollable and clickable while a question is pending.
- Multiple monitors: open the same workspace URL in two windows. Both get the
  same events; e.g. one shows the doc full-height, the other the code.

## Doc pane

The doc pane shows the document of the current question: the spec, or any
text file of the project that a doc ref names (07).

- **Documents outside a spec**: a doc ref outside the spec folder, or with no
  spec shown, is keyed by its absolute path. It is shown, but not watched and
  without a Changes tab.
- **Source documents**: a file that is not markdown (YAML, JSON, SQL) is
  rendered as code with Chroma, with the line range of the current question
  highlighted.
- **Line ranges**: a doc ref by `lines` tints the markdown blocks that
  overlap the range, as a section-only match does.
- **Document tabs**: when the current question has more than one document,
  tabs in the pane header switch between them.
- **A question with no document**, in a set that shows the pane, keeps the
  last document without its highlight. The header says "This question has
  no document" in a warning-coloured badge, and a dashed "No document" pill
  sits under the question text, where the chips of the other questions are.
  The pane mod shows the same two notes.
- The Changes tab shows only while a spec is shown.

- **Render**: goldmark with GFM (tables, task lists, strikethrough), heading IDs
  from the heading path (`design--retry-policy`), safe HTML only (no raw HTML
  passthrough).
- **Section index**: walk the AST once per load; record each heading's level,
  text, path and source byte range of its section.
- **Change detection**: hash each section's source by heading path. On reload,
  compare with the previous hashes; sections that are new or changed get a
  `changed` class (left bar + "◆ changed" badge) until the next reload or 5 min.
  Union this with `changed_sections` / `changed` from `spec_show`.
- **Changes tab**: next to the rendered spec, a "Changes (n)" tab shows what
  changed in the shown document. It compares with the document as it was
  when the user last replied to a question set (default), or when the
  session started. Both views show the whole document: "Rendered" marks added
  blocks green and removed blocks red; "Source" shows a line diff of the
  markdown. "Rendered" compares top-level blocks, list items and table rows:
  a table renders once, with removed rows red and added rows green, and is
  replaced as a whole only when its header changes. Opening the tab scrolls to the first change. The tab always
  shows the document the Spec tab shows: when requests finish out of order,
  only the newest one fills the pane.
  The daemon keeps these versions in memory, so no git is needed. The
  "Spec finished" banner links to all changes in the session.
- **Zoom**: a ⤢ button at the right of the doc pane header shows only the
  document, using the whole page. Clicking it again (⤡) or Esc restores the
  layout. A new question set restores it too, so a question never hides
  behind a zoomed document.
- **Show current passage**: a link in the doc pane header, shown while a
  passage is highlighted for the current question or a `focus` call. It
  scrolls back to that passage, switching documents first if needed.
- **Line count**: the doc pane header shows how many lines the document has
  ("· 214 lines"), from `lines` in the `doc` payload. It hides whenever the
  header would overflow, so it never pushes out the tabs or links.
- **Keep scroll position**: before a `doc` swap, `app.js` records the heading
  nearest the top of the pane and its pixel offset; after the swap it scrolls
  back to that heading. Without this, every Claude edit would jump to the top.
- **Debounce**: file events are coalesced (150 ms) before re-rendering.

### Quote highlighting

All matching happens on the **markdown source**, using `internal/specsrc` (the
same code as the bridge's pre-check), so there is one implementation:

1. **exact**: `strings.Index(source, quote)`.
2. **normalized**: normalize the source with an **index map** (normalized offset
   → source offset): NFC, case-fold, strip inline markdown syntax, collapse
   whitespace, map smart quotes and dashes. Search there, map the range back.
3. **fuzzy**: diff-match-patch `MatchMain` is limited to 32-character patterns,
   so locate the first 32 normalized characters (threshold 0.4), then the last
   32 near `start + len(quote)`, and take the span between. Accept if its length
   is within ±30% of the quote's.
4. **heading**: if a `section` was given, use that section's range.
5. **none**.

The result is a source byte range. `render/highlight.go` is a goldmark
`ASTTransformer` that walks text nodes, splits any `ast.Text` whose segment
crosses the range boundary, and wraps the covered pieces in a custom inline
`Highlight` node, rendered as `<mark class="q" data-ref="q1-0">`. A range that
spans paragraphs simply yields several `<mark>`s with the same `data-ref`. For
`heading` matches the section gets a lighter tint instead.

The match quality shows as a small badge on the ref chip (`≈ fuzzy`,
`§ section only`).

## Code pane

- Tabs, one per referenced file; label is the file name, title is the relative
  path.
- `render/code.go`: chroma with the lexer from the file name, `WithLineNumbers`,
  `WithLinkableLineNumbers` (anchors `L41`), `HighlightLines([[start,end]])`, and
  a theme pair for light/dark (`prefers-color-scheme`). Ranges are passed per
  request, so the same file can be shown with different highlights.
- `app.js` scrolls the first highlighted line to the vertical centre.
- Files are watched. On change, re-render and re-apply the current range. If the
  line count changed, show a "may be out of date" badge; the next `ask`/`focus`
  sends fresh ranges anyway.
- **Open in editor**: the document panel and each code tab have a link that
  asks the daemon (`POST /w/{ws}/open`) to open the file at the line. A plain
  link to an editor's URL scheme does nothing in a browser that does not
  hand such links to the system, such as the Claude desktop app's browser
  pane. The editor is a setting on the index page, stored in the browser:
  VS Code, Cursor and Zed open through their URL schemes with the platform's
  URL opener (`open`, `xdg-open`, `rundll32`); GoLand, which has no scheme
  for a file path, starts with `--line N <path>` (`open -na GoLand --args`
  on macOS, the `goland` launcher elsewhere). An error shows as a toast.

## Question panel

- Rendered server-side from the question set; one `<section>` per question,
  only the current one visible, with ‹ › and per-question chips (✓ when
  answered).
- Per question: text; ref chips (`§ <section>` for doc refs, prefixed with the
  artifact name when it isn't the primary file; `file:lines` for code refs);
  radio buttons or checkboxes (`multi_select`) with descriptions; an "Other"
  input on every question; notes textarea.
- **Showing a question auto-focuses its first doc ref and first code ref.**
  Clicking a chip focuses that ref (switch artifact/code tab, scroll to the
  `<mark>` or line range).
- Buttons POST via htmx: **Send answers** (enabled when every question has a
  selection or Other text), **Back to chat**, **Skip** (sent as `dismissed`).
- **Back to chat** also copies the question set to the clipboard
  (`navigator.clipboard.writeText`, which works on `127.0.0.1`): the title, then
  each question with its options as a numbered list, so it can be pasted into
  the reply. A toast confirms the copy.
- Each question has a **"keep as open question"** checkbox, shown only when the
  workspace root is a folder. Its value is sent with **Back to chat** and
  **Skip** and comes back to Claude as `keep_open` (01).
- Keyboard: `1`–`6` select an option, `Ctrl/Cmd+Enter` answers, `Alt+←/→`
  navigates questions.
- After answering, the panel collapses to a one-line summary ("Answered 2
  questions · 14:03"), and the Q&A goes into the **History** tab of the panel.
  Clicking a history entry re-focuses its refs.

## Notifications

When an `ask` arrives:
- the page title gets a `● ` prefix and the favicon a dot, until answered
- a browser `Notification` (permission asked once from the index page) with the
  project name and the first question; clicking it focuses the tab
- if no page is open for the workspace (`viewers: 0`), `ask` returns at once so
  Claude can reopen the built-in browser pane; without a pane the bridge opens
  the URL in the default browser after 10 seconds (01)

The daemon never steals focus beyond that.

## Tests

- `conn`: framing (partial lines, several messages in one read, oversized line).
- `workspaces`: isolation (two connections, interleaved messages), archive,
  adoption + redirect.
- `specsrc` (shared): quote fixtures; fuzzy acceptance and rejection; index map
  round-trips.
- `render/highlight`: quotes inside bold/code/links, across a line break, across
  two paragraphs; `heading` fallback. Golden HTML files.
- `render/markdown`: section index, change detection across two versions.
- `web`: `httptest` for routes, Host/Origin rejection, first-answer-wins `409`,
  SSE initial state on connect.
- Manual: the end-to-end script in 05.
