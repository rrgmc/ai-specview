# 01 — Protocol (the contract)

There are three interfaces:

1. **MCP tools**: what Claude sees (Claude ↔ `ai-contextview mcp`).
2. **Socket protocol**: bridge ↔ daemon (`ai-contextview mcp` ↔ `ai-contextview serve`).
3. **HTTP/SSE**: daemon ↔ browser. Internal to the daemon and its embedded UI,
   so it is described in 03, not versioned here.

1 and 2 are versioned. Both sides of 2 are Go, so the message structs live in
one shared package (`internal/protocol`). Golden fixtures in `protocol/fixtures/`
still pin the wire format, so a non-Go client can be written against them.

---

## 1. MCP tools

### `spec_show`

Declares or refreshes what is shown for this session: a single markdown file,
or a **folder of markdown artifacts** (a change folder, see 00).

```json
{
  "path": "docs/specs/retry-policy.md",
  "changed_sections": ["Retry policy", "Error budget"]
}
```

```json
{
  "path": "changes/004-jittered-retries",
  "primary": "proposal.md",
  "changed": [{"path": "adr/002-cap-resets-daily.md"}]
}
```

- `path` (required): a `.md`/`.markdown` file or a directory, relative to the
  Claude cwd or absolute. As in Claude Code's file tools, a leading `~` or
  `~/` is the user's home folder; on Windows, `~\` is too, and a Git Bash
  path such as `/c/Users/me` is `C:\Users\me`. The same holds for `primary`
  and for a doc ref `path`.
- `primary` (optional, folders only): the file shown first and used when a ref
  has no `path`. Default: the first of `proposal.md`, `spec.md`, `README.md`
  that exists, else the first file alphabetically.
- `changed_sections` (optional): heading texts the caller just changed in the
  primary file. Only a hint; the daemon computes changes itself (03).
- `changed` (optional, folders only): `[{path, sections?}]` for other files the
  caller just created or edited.
- `provisional` (optional, ask mode or spec mode only): show the file until
  the working document is written, for example a spec skill's exploration
  notes before the spec exists. It is shown like a document picked from a
  write: it does not stop the pick, it clears when ask mode ends, and it does
  not block the built-in question tool outside ask mode. The first written
  file that would become the working document replaces it; writes to the
  provisional file itself do not. Outside ask mode the bridge refuses it.
  The viewer and the pane label the document as provisional.

For a folder, the daemon includes every `*.md` file up to 3 levels deep, skipping
dot-files and dot-directories. New files that appear later are picked up by the
watcher.

Result:

```json
{"ok": true,
 "primary": "proposal.md",
 "sections": ["Overview", "Retry policy"],
 "artifacts": [
   {"path": "proposal.md", "kind": "spec", "sections": ["Overview", "Retry policy"]},
   {"path": "adr/002-cap-resets-daily.md", "kind": "adr", "sections": ["Context", "Decision"]}
 ]}
```

The result also has `url`, the viewer page of the session, and by default
`"open": "pane"`: the caller opens `url` in the built-in browser pane of the
Claude desktop app, when that pane is available. `AI_CONTEXTVIEW_OPEN=browser` or
`never` leaves `open` out. Without the pane, the bridge opens the default
browser when an `ask` finds no open page (see `ask_shown` with `viewers: 0`).

`sections` is always the primary file's headings, so single-file callers can
ignore `artifacts`. Returning headings lets Claude use exact section names
later.

**Artifact kind** is inferred, first match wins:
1. frontmatter `kind:` (`spec | adr | research | open_question | other`)
2. any parent directory named `adr`/`adrs`/`decisions` → `adr`;
   `research` → `research`; `open-questions`/`questions` → `open_question`;
   `specs` → `spec`
3. file name `proposal.md`, `spec.md`, `design.md` → `spec`
4. `other`

Errors: the path doesn't exist; a file that isn't markdown; a folder with no
markdown files; `primary` not inside the folder.

### `focus`

Highlights something without asking anything.

```json
{
  "spec_quote": "retries use exponential backoff capped at 30s",
  "doc_refs": [{"path": "adr/002-cap-resets-daily.md", "quote": "the cap resets at midnight UTC"}],
  "code_refs": [{"path": "internal/sync/retry.go", "symbol": "sync.(*Retrier).Next"}]
}
```

Result: `{"ok": true, "docs": [{"path": "...", "match": "exact|normalized|fuzzy|heading|none"}], "code": [ResolvedRef...]}`

`spec_match` is also returned, equal to the match of the first doc ref, for
callers that only use `spec_quote`.

### `ask`

Shows a question set and **blocks** until the user acts or the wait limit is
reached.

```json
{
  "title": "Retry semantics",
  "questions": [
    {
      "id": "q1",
      "text": "Should retries reset the backoff on partial success?",
      "options": [
        {"label": "Reset", "description": "Start again from the base delay", "recommended": true},
        {"label": "Keep growing"},
        {"label": "Configurable"}
      ],
      "multi_select": false,
      "spec_quote": "retries use exponential backoff capped at 30s",
      "spec_section": "Retry policy",
      "doc_refs": [
        {"path": "adr/002-cap-resets-daily.md", "quote": "the cap resets at midnight UTC", "section": "Decision"}
      ],
      "code_refs": [
        {"path": "internal/sync/retry.go", "symbol": "sync.(*Retrier).Next"},
        {"path": "internal/sync/client.go", "lines": "88-104"}
      ]
    }
  ]
}
```

`spec_quote` + `spec_section` are shorthand for a doc ref into the primary file.
Internally the bridge turns them into `doc_refs[0]`. Both forms may be used
together.

Rules, enforced by the bridge, which returns an MCP tool error so Claude
retries:
- 1–6 questions; each has 2–6 options. At most one option per question has
  `recommended: true`. A label ending in "(Recommended)" is accepted too: the
  bridge strips the suffix and sets the flag.
- Each question has **at least one of** `spec_quote`, `doc_refs` or `code_refs`,
  except in ask mode (see `ask_mode`), where refs are optional. The daemon
  accepts a question with no refs; the bridge enforces this rule.
- At most 4 doc refs and 4 code refs per question.
- Each quote is 3–300 characters and must pre-match its file's source (normalized
  match, see 02). If it doesn't match, the error names the file and lists its
  closest headings.
- A doc ref names a document: any text file of the project. Its `path` is
  relative to the `spec_show` folder when the file is there, else to the
  project directory, or absolute; omitted means the primary spec file, which
  needs a spec shown. The file must exist.
- A doc ref has exactly one of `quote` or `lines` (`N` or `N-M`). A quote
  needs a markdown file; a file that is not markdown takes `lines`. A range
  past the end of the file is cut at the last line; a start past the end is an
  error.
- Each code ref has `path` and exactly one of `symbol`/`lines`. A symbol that
  can't be resolved is an error that names the candidates found.

Result, one of:
```json
{"status": "answered",
 "answers": [{"id": "q1", "selected": ["Keep growing"], "other": null, "notes": "but cap resets daily"}]}

{"status": "in_chat", "keep_open": ["q2"],
 "message": "The user will answer in the chat. Wait for their message."}

{"status": "pending", "question_set_id": "qs_7f3a",
 "message": "Still waiting for the user. Call ask_wait with this id to keep waiting."}

{"status": "pending", "question_set_id": "qs_7f3a", "url": "http://127.0.0.1:7878/w/ws_A",
 "message": "No viewer page is open, so the user cannot see these questions. …"}

{"status": "ask_in_chat", "question_set_id": "qs_7f3a",
 "message": "The user is away from the viewer, so ask these same questions in the chat now, with the built-in question tool …"}

{"status": "dismissed", "keep_open": ["q1"],
 "message": "The user skipped these questions. Continue without these decisions; ask in chat only if one is essential."}
```

`ask_in_chat` means nobody is at the viewer, for example because the user
follows the session over Remote Control from another device, where a
`127.0.0.1` page cannot open. Claude asks the same questions in the chat with
the built-in question tool, which `ai-contextview hook` allows while the
questions are in the chat (`ask_here`). `ask` returns it at once while they
are in the chat, and also at the nudge (Failure modes) unless
`AI_CONTEXTVIEW_AWAY=wait`.

`deferred` (optional, `answered` only) lists the questions the user chose
**Decide later** for. Each also appears in `answers` with `"deferred": true`,
an empty `selected` and the user's `notes`, if any. Claude records them as open
questions and asks them again before finishing.

A question whose own options already offer to decide later (an option labelled
"Decide later", "Decide this later", "Defer" or "Later", ignoring case and
punctuation) gets no second **Decide later** choice in the viewer or the pane.
Choosing that option is an ordinary answer: it appears in `selected`, not in
`deferred`.

`keep_open` (optional, `in_chat` and `dismissed` only, older viewers) lists the questions the
user ticked "keep as open question" for. In a change folder the skill records
each of them as an `open-questions/` file (04, M8). Omitted when empty.

### `ask_wait`

`{"question_set_id": "qs_7f3a"}` → same result shape as `ask`. It exists because
an MCP tool call can't block forever (see the timeouts in 02).

### `ask_here`

Moves the session's questions to the chat, for a user away from the viewer,
or back to the viewer with `{"off": true}`. Result:
`{"in_chat": bool, "closed"?: "qs_…", "message"}`. A question set waiting in
the viewer is closed (`closed`); Claude asks it again in the chat. Ask mode
and an open spec stay as they are. The questions come back to the viewer by
themselves when someone uses a viewer page (`presence` with `active=1`). The
`ask-here` skill (`/ai-contextview:ask-here`) calls it; the pane mod has no
such tool (06).

### `ask_mode`

Turns ask mode on or off for the session. In ask mode every question of the
session goes through `ask`, with or without a spec.

```json
{}
{"spec": true, "paths": ["docs/specs/**"]}
{"off": true}
```

- With no input (or `off: false`) it turns ask mode on; `off: true` turns it
  off.
- **Off hands the questions back to the chat.** Turning ask mode off also
  releases an open spec: the daemon no longer counts the session as active,
  so `ai-contextview hook` stops blocking the built-in question tool, even
  while a `spec_show` spec is open and not done. The spec stays in the
  viewer. The tool result has `next`, which tells Claude to ask in the chat
  from now on. Turning ask mode on again ends the release, and so does a
  `spec_show` of another spec (a new spec session). A `spec_show` of the
  released spec, as after an edit, keeps it released.
- `spec: true` turns on **spec mode**: ask mode that expects a spec. `paths`
  (optional, spec mode only) are glob patterns of the spec file, relative to
  the project directory or absolute; `*` and `?` match within a folder, `**`
  any folders. The bridge makes them absolute. A path with no glob
  characters (`*`, `?`, `[`) that names an existing markdown file becomes the
  working document at once, as a pick (see below), unless Claude's own
  `spec_show` is shown: a spec written before spec mode started, as when
  Claude implements it, needs no write to show. The result then has
  `shown`, its absolute path. A relative path with no glob that names no
  file under the session's folder is looked up in the other git worktrees
  of the session's repository (and of the folder the bridge started in),
  found by reading their `.git` files; when several have it, the most
  recently changed copy is used. With no copy anywhere, the path stays
  relative to the session's folder.
- **The working document.** In ask mode, the markdown files Claude writes are
  reported by `ai-contextview hook` (04) and one of them becomes the working
  document, as if `spec_show` had shown it (07, "Finding the working
  document"):
  - ask mode: the first written file;
  - spec mode with `paths`: the first written file that matches;
  - spec mode without `paths`: the first written file that scores at least 2:
    a spec skill started (1), a spec folder (`specs`, `rfc`, `design`, `adr`,
    …, or `~/.claude/specs` and `~/.claude/plans`) (1), spec headings or
    `kind: spec` frontmatter (1), written at least twice (1).

  After that, later writes do not switch it, but an `ask` whose doc refs name
  another written file does. Claude's own `spec_show` always wins: once it
  is called, writes are ignored, unless it was called with `provisional`.
  Turning ask mode off forgets the pick and
  clears a picked document from the viewer, which goes back to its state
  without a document; the bridge forgets it too, so a reconnect does not show
  it again. A spec shown with `spec_show` stays.
- While it is on, `ask` accepts questions with no refs, the daemon counts the
  session as active, so `ai-contextview hook` blocks the built-in question tool
  even with no spec open (04), and the spec pane of an empty viewer says that
  ask mode is on.
- `spec_done` does not end ask mode.
- The bridge replays it after a reconnect.

Result: `{"on": true, "url": "...", "open": "pane"}` when turned on (`url` and
`open` as in `spec_show`; `"spec": true` in spec mode), `{"on": false}` when
turned off. In spec mode, `"shown": "<abs path>"` when a path named an
existing spec, now the working document.

### `spec_done`

Tells the viewer the spec is finished and no more questions are coming.

```json
{"summary": "**Decided**\n\n- Retries reset on partial success.\n- The cap stays at 30s.\n\n**Still open**\n\n- The jitter range."}
```

- `summary` (optional): short markdown, at most 8000 bytes. The viewer renders
  it in a "Spec finished" banner.
- A pending question set is withdrawn (`dismissed`).
- Highlights (from `focus` or questions) are cleared and the code tabs close.
- A later `ask` clears the finished state.

Result: `{"ok": true, "url": "..."}`.

### Shared types

```
CodeRef        = { path: string, symbol?: string, lines?: "N" | "N-M" }
ResolvedRef    = { path: abs string, start_line: int, end_line: int,
                   symbol?: string, kind?: "func"|"method"|"type"|"var"|"const",
                   source: "symbol"|"lines" }
DocRef         = { path?: string, quote?: string, lines?: "N" | "N-M",
                   section?: string }   (exactly one of quote or lines)
ResolvedDocRef = { path: abs string, quote?: string, start_line?: int,
                   end_line?: int, section?: string, kind: ArtifactKind }
Artifact       = { path: string (relative to root), kind: ArtifactKind,
                   sections: [string] }
ArtifactKind   = "spec" | "adr" | "research" | "open_question" | "other"
Match          = "exact" | "normalized" | "fuzzy" | "heading" | "none"
```

---

## 2. Socket protocol (bridge ↔ daemon)

### Transport

- Unix socket at a fixed path: `$XDG_RUNTIME_DIR/ai-contextview.sock`, or on macOS
  `~/Library/Caches/ai-contextview/ai-contextview.sock`, or on Windows
  `%LOCALAPPDATA%\ai-contextview\ai-contextview.sock`. Override with `$AI_CONTEXTVIEW_SOCKET`;
  keep the path under about 100 bytes, the Unix socket limit.
  The daemon creates it with mode `0600`.
- Framing: **NDJSON**: one JSON object per line, UTF-8, `\n` terminated. Maximum
  line length 4 MiB.
- One connection per bridge; that connection is the workspace.

### Envelope

```json
{"v": 1, "type": "ask", "id": "m_12", "ws": "ws_4c1e…", "body": { … }}
```
- `id` is unique per sender. Replies set `"re": "<id>"`.
- `ws`: workspace ID, a UUIDv4 generated by the bridge at startup.

### Messages: bridge → daemon

| type | body | reply |
|---|---|---|
| `hello` | `{protocol: 1, ws, cwd, claude_pid, bridge_pid, bridge_version, tty?, project_name}` | `hello_ack {daemon_version, url, adopted_from?}` |
| `spec_show` | `{root_abs, is_dir, primary?, changed_sections?, changed?: [{path, sections?}], auto?: bool, provisional?: bool}` (`auto` marks the replay of a working document the daemon picked, which does not count as Claude's own `spec_show`; `provisional` shows the file until the working document is written, see the `spec_show` tool; the two are exclusive) | `result {primary, sections[], artifacts[]}` or `error` |
| `focus` | `{docs: [ResolvedDocRef], code: [ResolvedRef]}` | `result {docs: [{path, match}]}` |
| `ask` | `{qs_id, title?, questions: [ {…, docs: [ResolvedDocRef], code: [ResolvedRef]} ]}` | `ask_shown {matches}` right away, **then later** `ask_result`; while the questions are in the chat, `ask_shown {in_chat: true}` and nothing is shown (the set is listed in the history as asked in chat) |
| `ask_cancel` | `{qs_id, reason}` | `result` |
| `done` | `{summary?}` | `result {}` |
| `viewers` | `{}` | `result {viewers: int, hidden?: int, seen?: bool, presence?: bool}` (open browser pages of the workspace; `hidden` of them report they are hidden; `seen` when someone used a page since the pending set was shown; `presence` when the daemon tracks `hidden` and `seen`) |
| `ask_mode` | `{on: bool, spec?: bool, paths?: [abs glob]}` | `result {shown?}` |
| `ask_here` | `{on: bool, auto?: bool}` (`auto` when the bridge moves unseen questions; refused, with `in_chat: false`, if someone used a page since the pending set was shown) | `result {in_chat: bool, closed?: qs_id}`; a pending set closes with `ask_result {status: "ask_in_chat"}` |
| `cwd` | `{}`, sent before the bridge resolves a relative path | `result {cwd?: abs}` (the session's folder from the hook; empty when none was reported) |
| `bye` | `{}` | (none; the bridge closes) |

In `ask`, `spec_quote`/`spec_section` have already been folded into `docs` by
the bridge, so the daemon only sees `docs`.

### Messages: hook → daemon

`ai-contextview hook` opens its own short connection, sends no `hello`, and finds
its session by its ancestor processes, as `active` does.

| type | body | reply |
|---|---|---|
| `active` | `{pids: [int], cwd?}` | `result {active, ws?, url?, ask_mode?, in_chat?}` (`in_chat`: the hook allows the built-in question tool) |
| `doc_written` | `{pids: [int], path: abs, cwd?}` (a markdown file the hook's file rules let through) | `result {shown: bool, path?}` (`shown` when it just became the working document) |
| `skill_started` | `{pids: [int], name, cwd?}` | `result {}` |
| `session_cwd` | `{pids: [int], cwd}`, sent before each of ai-contextview's own MCP tools runs | `result {}` |

`cwd` is the session's working folder from the hook's input. In a desktop-app
worktree session it is the worktree, while the bridge was started in the main
checkout. The daemon keeps it and, when it changed, tells the bridge
(`session_cwd` below), which then resolves relative paths (doc and code refs,
`spec_show`, spec-mode `paths`) against it. The hook's file rules also judge
a written file by the folders below `cwd`, so the worktree's own
`.claude/worktrees/…` folder does not count as a dot-folder.

The hook reports the folder just before the tool runs, which can be before
the session's bridge connected (its first ai-contextview call). The daemon then
keeps the folder by the hook's ancestor processes for up to ten minutes and
gives it to the workspace whose `claude_pid` or `bridge_ppid` matches at
`hello`. The unasked `session_cwd` can also reach the bridge after the tool
call it was meant for. So, before it resolves a relative path, the bridge
asks with `cwd` and uses the answer.

### Messages: daemon → bridge

| type | body |
|---|---|
| `hello_ack` | `{daemon_version, url, adopted_from?: ws}` (`url` is the workspace page, e.g. `http://127.0.0.1:7878/w/ws_A`) |
| `result` | `{…}` (reply to `re`) |
| `error` | `{code, message}` (reply to `re`) |
| `ask_shown` | `{qs_id, viewers: int, hidden?: int, matches: [{id, doc: int, match}]}` (`doc` indexes the question's `docs`; `viewers` is the number of open browser pages, `hidden` how many of them are hidden) |
| `ask_result` | `{qs_id, status: "answered"\|"in_chat"\|"dismissed", answers?, deferred?: [id], keep_open?: [id]}` |
| `session_cwd` | `{cwd}`: the session's working folder changed (from the hook). |
| `doc_shown` | `{root_abs}`: the daemon made a written file the working document. The bridge loads it as its spec root, so `spec_quote` quotes it, and replays it after a reconnect as `spec_show` with `auto: true`. |

Error codes: `bad_request`, `file_not_found`, `not_markdown`, `empty_folder`,
`unknown_qs`, `busy` (another question set is pending in this workspace),
`internal`.

### Sequencing rules

- **At most one pending question set per workspace.** A second `ask` replaces
  the first: the daemon sends `ask_result{status:"dismissed"}` for the old one,
  then shows the new one. This handles Claude giving up on a question and asking
  again.
- **`ask_shown` with a `none` match** triggers an `ask_cancel` from the bridge,
  which returns a tool error to Claude ("quote not found in <file>; closest
  sections: …"). Bridge and daemon use the same matching package, so this only
  happens if the file changed between the pre-check and the show.
- **`ask_shown` with `viewers: 0`**: no page shows the questions, so a question
  never waits on a page nobody is looking at. What the bridge does depends on
  `AI_CONTEXTVIEW_OPEN`:
  - `pane` (the default): `ask` returns at once with `pending`, the
    `question_set_id` and the workspace `url`, so Claude can reopen the
    built-in browser pane (for example after the user closed it) and then call
    `ask_wait`. After 10 seconds the bridge sends `viewers`; if no page is open
    by then and the set has no result, it opens `url` in the default browser.
    That is the fallback when there is no pane (the CLI, an IDE).
  - `browser`: the bridge opens `url` in the default browser at once and
    waits as usual.
- **`ask_shown` with every viewer hidden** (`hidden` equal to `viewers`, for
  example a closed browser pane whose tab is still open): `ask` returns at
  once with `pending` and `url`, and its message tells Claude to say in the
  chat that questions are waiting and to show the pane again. No browser is
  opened.
- **The nudge.** Otherwise `ask` waits for the answers. After 45 seconds
  without an answer it sends `viewers`; if the daemon tracks presence and
  `seen` is false (nobody used a page since the questions were shown), the
  bridge sends `ask_here {on: true, auto: true}` and `ask` returns
  `ask_in_chat`: Claude asks in the chat. With `AI_CONTEXTVIEW_AWAY=wait`, or
  a daemon that refuses `ask_here`, `ask` returns `pending` with `url`
  instead, and its message tells Claude to say in the chat that questions
  are waiting, then call `ask_wait`. `ask_wait` never nudges. Pages report what they know with `POST /w/{ws}/presence` (03).
  - `never`: the bridge opens nothing and waits as usual.
- **If the daemon restarts,** the bridge reconnects, re-sends `hello` with the
  same `ws`, re-sends the last `spec_show` (or `doc_shown`), the last `ask_mode`, any pending
  `ask`, and the last `done` (unless an `ask` came after it).
- **Disconnect** (`bye` or EOF): the daemon archives the workspace (03).

### Example session

```
→ {"v":1,"type":"hello","id":"b1","ws":"ws_A","body":{"protocol":1,"cwd":"/Users/r/src/shop",...}}
← {"v":1,"type":"hello_ack","re":"b1","body":{"daemon_version":"v0.1.0","url":"http://127.0.0.1:7878/w/ws_A"}}
→ {"v":1,"type":"spec_show","id":"b2","ws":"ws_A","body":{"root_abs":"/Users/r/src/shop/docs/specs/retry.md","is_dir":false}}
← {"v":1,"type":"result","re":"b2","body":{"primary":"retry.md","sections":["Overview","Retry policy"],"artifacts":[…]}}
→ {"v":1,"type":"ask","id":"b3","ws":"ws_A","body":{"qs_id":"qs_1","questions":[…]}}
← {"v":1,"type":"ask_shown","re":"b3","body":{"qs_id":"qs_1","viewers":1,"matches":[{"id":"q1","doc":0,"match":"exact"}]}}
   … user browses, clicks …
← {"v":1,"type":"ask_result","body":{"qs_id":"qs_1","status":"answered","answers":[…]}}
```

## Versioning

- `v` is the envelope version and `protocol` (in `hello`) is the message-set
  version. The daemon rejects a `hello` with an unknown major version, and the
  bridge then returns a tool error telling the user to update. Since bridge and
  daemon ship in one binary this only happens when an old daemon is still
  running; the error says to run `ai-contextview serve --restart`.
- Adding new optional fields is allowed without a version bump. Both sides
  ignore fields they don't know. (`doc_refs`, folder `spec_show`, `viewers` and
  `url` were added this way before any release, so protocol stays at 1.)
