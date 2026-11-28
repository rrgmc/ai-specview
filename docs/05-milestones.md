# 05 — Milestones, testing, risks

Each milestone ends with something runnable. M1 and M2 can proceed in parallel
once M0 is done. M0–M7 are the first version; M8–M9 are the change-folder workflow (00
"Future").

## Status

| Milestone | State |
|---|---|
| M0–M5 | Done. |
| M6 | Done, except the dogfood session with 3+ rounds on a real project. |
| M7 | Done. `internal/e2e/multi_test.go` is the scripted three-session test. |
| M8, M9 | Not started. |
| Pane mod | Prototype in `plugin/` with the `spec-elaborate-mod` skill (06). It serves its own tools, questions and doc refs included, with no daemon. |

What changed from the plan while building and using it:

- **Five tools, not four.** `spec_done` marks the spec finished. The page
  shows a "Spec finished" banner with the summary (a **Decided** and a
  **Still open** bullet list), clears the highlights and closes the code tabs.
- **Panel buttons.** "Send answers", "Back to chat" (was "Answer in chat") and
  "Skip" (was "Dismiss"). Clicking an option text answers and moves to the next
  question; clicking the radio button waits for "Next". After the last question
  a review step lists all answers before sending.
- **"Decide later" replaced the "keep as open question" checkbox.** It is one
  more choice per question. Claude gets the deferred question ids
  (`deferred` in the result) and records them as open items in the spec.
  `keep_open` stays in the protocol for "Back to chat" and "Skip".
- **Ask mode.** The `ask-mode` and `ask-mode-mod` skills turn on a mode
  (`ask_mode` tool and message) that routes every question of a session
  through ai-contextview, with or without a spec. Refs are optional in it, and
  the hook blocks the built-in question tool while it is on.
- **A closed viewer is reopened.** When an `ask` finds no open page (the user
  closed the browser pane), it returns `pending` with the `url` at once, so
  Claude reopens the pane and then calls `ask_wait`. A new `viewers` message
  lets the bridge check 10 seconds later and open the default browser if
  still no page is open.
- **Questions follow the user to the chat.** The viewer is a loopback page,
  so a user on Remote Control from another device cannot open it. When a
  question goes unseen for the nudge delay, the bridge moves the session's
  questions to the chat (`ask_here`): `ask` returns `ask_in_chat`, the hook
  allows the built-in question tool, and the page shows "Questions in chat".
  Using the page moves them back. The `ask-here` skill does the same on
  request; `AI_CONTEXTVIEW_AWAY=wait` keeps the old nudge. The pane mod has
  no presence signal, so it is not covered.
- **Recommended options.** An option can carry `recommended: true` (or a
  "(Recommended)" label suffix, which the bridge turns into the flag); the
  panel marks it.
- **Changes tab.** The doc pane has Spec and Changes tabs. Changes shows the
  whole document as a block diff (rendered) or a line diff (source), against
  the last answers or the start of the session. This settles the former open
  question about a spec diff. The tab always shows the document the Spec
  tab shows; a slow, older request no longer replaces a newer one.
- **"Show current passage" link** next to "open in editor" scrolls back to the
  highlighted passage.
- **Layout.** Spec on the left; questions on the right with the code below them.
- **The built-in question tool is blocked by a hook, not a deny rule.**
  `ai-contextview hook` blocks `AskUserQuestion` only in the Claude session that has
  an unfinished spec open, so other sessions keep it (04, section 2).
- **Install.** The module path is the repository path, and the MCP server,
  skill and hook ship as a Claude Code plugin (README).
- **Pane mod.** The plugin also holds a Claude Code mod that draws the spec,
  the code and the questions in a pane beside the chat, with its own tools and
  its own skill, `spec-elaborate-mod` (06).
- **Draft specs.** By default, both skills write a new spec to
  `~/.claude/specs/<project>/<slug>.md` instead of `docs/specs/`, so it stays
  out of the repository like a plan-mode plan and needs no branch. They use
  `docs/specs/` only when the user asks for the repository. Before
  `spec_done` they ask whether to move it into `docs/specs/`. Drafts were
  opt-in at first and became the default after v1.0.0. The bridge's tools
  and the pane's tools expand a leading `~/` as Claude Code's file tools do,
  so the skills pass the draft path with its `~` on every platform and need
  no shell to find the home folder. Code refs stay relative to the session's
  folder.
- **Viewer in the desktop app's browser.** By default `spec_show` returns
  `open: "pane"` and the `spec-elaborate` skill opens the viewer `url` in the
  Claude desktop app's built-in browser. `AI_CONTEXTVIEW_OPEN=browser` turns
  this off. The bridge still opens the default browser when no page is open
  at an `ask`, so the CLI and IDEs work as before.
- **Versions come from git tags.** The binary has no version constant. It
  reads the version Go records at build time: the module version for
  `go install …@v1.2.3`, and the tag, a pseudo-version or `+dirty` for a build
  in a checkout. `-ldflags "-X main.version=…"` overrides it, and a build with
  no version information is `devel`. A release is a `vX.Y.Z` tag on
  `master`. Pushing the tag runs the `Release` workflow: it checks that `changelog.d/` was merged into
  `CHANGELOG.md` (`tools/changelog`), runs the tests, builds Linux, macOS and
  Windows binaries and creates the GitHub release. The `CI` workflow tests every push
  to `master` and every pull request, except pushes that only change markdown
  files or `docs/`, such as a release commit.
- **Windows.** The binary builds and its tests pass on Windows, which has
  Unix sockets since Windows 10 1803. Platform code sits behind build tags:
  the instance lock (`flock` / `LockFileEx`), detached daemons (`setsid` /
  `DETACHED_PROCESS`) and the process tree (`ps` / a Toolhelp snapshot). The
  plugin's `sh` launcher runs the hook there too: Claude Code starts hooks
  with Git Bash, which finds `ai-contextview.exe` as `ai-contextview`, and the
  launcher reads a `;`-separated Windows `GOPATH`. The MCP server starts
  without Git Bash, and `sh` is often not on the Windows `PATH`, so Claude Code
  runs `scripts/ai-contextview.cmd` there instead: a batch twin of the `sh`
  launcher with the same search. Releases include Windows binaries (amd64 and arm64, as
  `.zip`); CI does not test on Windows yet.
- **Front end.** Plain JavaScript instead of htmx; the web server lives in
  `internal/daemon`. Browser tests in `internal/daemon/browser_test.go` drive
  the panel in headless Chrome.
- **Spec mode shows an existing spec.** A spec-mode path that names an
  existing markdown file shows it at once, in the viewer and the pane, so a
  session that implements a written spec sees it without writing to it
  (01, `ask_mode`; 07). A relative path missing from the session's folder
  is looked up in the repository's other git worktrees
  (`internal/gitwt`), newest copy first.

## M0 — Contract and skeleton

- Repo layout from the README: one `go.mod`, `cmd/ai-contextview` with empty `mcp`,
  `serve`, `fakeapp` subcommands that build.
- `internal/protocol`: envelope and message structs for 01, including
  `DocRef`/`ResolvedDocRef`/`Artifact`.
- `protocol/fixtures/`: at least one valid and one invalid example per message.
- `protocol/quotes/`: the quote-matching fixtures (markdown source, quote,
  expected match quality and source range).
- MCP tool input schemas come from the Go structs via the go-sdk; no
  hand-written JSON Schema.

**Done when:** a test loads every fixture and round-trips it through the
structs.

## M1 — Bridge end to end with the fake daemon

- `ai-contextview mcp` with all tools (four planned; `spec_done` came later),
  validation, `lines`-only code refs (no symbols yet), the `specsrc` quote
  pre-check (single file and folder roots, `doc_refs`), `appconn` with daemon
  start and reconnect.
- `ai-contextview fakeapp`, which prints messages and prompts for answers in the
  terminal.
- Register it in a scratch project with `AI_CONTEXTVIEW_DAEMON="ai-contextview fakeapp"`;
  ask Claude to call `ask` with a sample question.

**Done when:**
- a real Claude Code session calls `ask`, the answer typed into fakeapp comes
  back as the tool result, and Claude continues
- `pending` → `ask_wait` works (with `AI_CONTEXTVIEW_ASK_WAIT=10s`)
- killing fakeapp mid-question and restarting it re-delivers the question

## M2 — Daemon: socket, workspaces, doc pane

- `daemon` socket server, `Conn`, workspaces, single-instance guard, stale socket
  cleanup, `--detach`.
- `web` server with Host/Origin checks, index page, workspace page, SSE hub.
- `render/markdown`: goldmark rendering, section index, file watcher (re-add after
  atomic saves, debounced), scroll position kept across reloads.
- `spec_show` handling for a single file **and** a folder (artifact sidebar).

**Done when:** two Claude sessions in two projects each get their own workspace
page showing their own spec, and editing the spec in an editor (or by Claude)
updates the right page without jumping the scroll.

## M3 — Code pane

- `render/code`: chroma with line numbers, line anchors, highlighted ranges,
  light/dark themes; code tabs; reload on change; "open in editor" links.
- `focus` end to end with `lines` refs.

**Done when:** `focus` with two code refs opens two tabs with the right ranges
highlighted and centred.

## M4 — Question panel and blocking ask

- Question panel: stepper, chips, options, Other, notes, Decide later, a
  review step, three buttons (Send answers, Back to chat, Skip), keyboard
  shortcuts. POST handlers with first-answer-wins.
- Auto-focus of refs per question; `ask_shown` (with `viewers`) / `ask_result`;
  bridge opens the URL when nobody is watching.
- Replacing a pending question set (`dismissed` for the old one).
- History tab.
- Notifications: title/favicon badge, browser `Notification`.

**Done when:** the full loop works from a real Claude session: question shown,
spec and code highlighted, the user browses freely, answers, and Claude
receives the answers. The same works with "Back to chat".

## M5 — Quote matching and highlighting

- `specsrc.Match` stages exact → normalized (with the index map) → fuzzy (the
  32-character anchors) → heading, on markdown source.
- `render/highlight`: the goldmark AST transformer that wraps the matched source
  range in `<mark>`s, including ranges across inline formatting and paragraphs.
- Match-quality badge on ref chips; a `none` match → `ask_cancel` → tool error.
- Section change detection (hash per heading path) with ◆ changed markers.

**Done when:** all quote fixtures pass, and a quote containing bold/code/link
text and a line break still matches `exact` or `normalized` and is highlighted
in the right place. Bridge and daemon pass the same fixtures by construction
(same package).

## M6 — Go symbols and skill

- `resolve` with `go/packages`, caching, near-miss suggestions.
- Example skill `plugin/skills/spec-elaborate/`, CLAUDE.md snippet, and the
  `ai-contextview hook` that blocks the built-in question tool while a spec is open.
- Dogfood: elaborate a real spec for one of your Go projects.

**Done when:** a dogfood session with at least 3 question rounds needs no manual
navigation; every question lands on the right spec text and code.

## M7 — Multi-instance hardening and install

- Archive on disconnect, auto-adopt on resume (same cwd, < 2h) with page
  redirect, the archived workspace limit.
- `ai-contextview serve --restart`; version mismatch message between an old daemon and
  a new bridge.
- Optional idle exit.
- Installation notes in the repo README (`go install`, the plugin,
  `MCP_TOOL_TIMEOUT`).

**Done when:** a scripted test with three concurrent Claude sessions, daemon
restarts, and a `claude --resume` shows no cross-talk and no lost pending
questions.

**Result:** `TestMultiSession` builds the binary and runs three `ai-contextview mcp`
processes in three projects, as three Claude sessions would. The bridges start
the daemon themselves. With a question pending in each, the test restarts the
daemon with `serve --restart`, then kills it with `kill -9`. Each page must show
only its own question, with the same id, and each answer must reach its own
session. Last, one session exits and a new bridge in the same directory takes
its workspace over, with its history.

The test found a bug: after a crash, all bridges start a daemon at the same
moment, and two could end up running, each on its own port. Each removed the
stale socket and listened, so the second removed the first one's socket. Now a
daemon takes an exclusive lock on `<socket>.lock` before it listens, and the
others exit.

Known limit: the daemon keeps its state in memory. After a restart the bridges
show their spec and pending question again, but the answer history and ended
sessions are gone, so `claude --resume` after a daemon restart starts a new
workspace.

## M8 — Change folders

- An OpenSpec custom schema (`openspec schema init`) that adds `adr/`,
  `research/` and `open-questions/` artifacts to OpenSpec's proposal / specs /
  design / tasks.
- Skill: a "start task" step that creates a numbered branch and
  `openspec/changes/<NNN-name>/` from that schema, then calls `spec_show` on the
  folder.
- Skill: answered questions that settle a design choice become ADR files
  (MADR-style, `kind: adr`). Questions the user marked "Decide later"
  (`deferred` in the result, 01) become `open-questions/` files. Research notes the skill writes go in
  `research/`.
- Skill rule: use `doc_refs` when a question depends on an ADR, research note or
  another spec.
- Viewer: artifact sidebar grouped by kind, ◆ changed per artifact, open-question
  count in the header.
- Bridge: if the `openspec` CLI is on the PATH, call
  `openspec status --change <name> --json` on `spec_show` and pass the phase and
  task progress to the viewer header. Without the CLI, everything still works
  from the folder alone.

**Done when:** a dogfood session in a change folder produces at least one ADR
and one open question, and a question with a `doc_refs` entry into an ADR
highlights the ADR next to the proposal and the code.

## M9 — Handoff from a merged plan

- A final skill command that reads a merged change folder and produces the
  handoff for the next agent: a Jira epic with stories (or an RFC), linking back
  to the change folder in Git. Existing epic and RFC skills can cover most of
  the output side.
- The open questions that remain are carried into the epic or RFC as explicit
  open items, not dropped.

**Done when:** one real task goes branch → plan MR → merge → Jira epic, and an
implementation agent can start from the epic without re-asking the questions
already answered in the ADRs.

---

## End-to-end manual test script

1. Start Claude in project A with the skill. Ask it to spec "add jittered
   retries". The bridge opens `http://127.0.0.1:7878/w/…` in the browser and the
   spec renders.
2. Claude edits the spec: the page updates in place and the edited section shows
   ◆ changed.
3. Claude asks 2 questions: the panel shows Q 1/2; the spec quote and
   `Retrier.Next` are highlighted; the title shows ● and a notification appears.
4. Scroll the spec away, switch code tabs, open Q 2/2, click Q1's chip: the view
   re-focuses Q1's refs.
5. Answer. Claude applies the answer, and the Decisions section gets ◆ changed.
6. Start Claude in project B at the same time and ask a question there: it shows
   on B's page only, and A's page is unchanged.
7. Kill `ai-contextview serve` while B has a question pending: the bridge restarts it,
   B's page reconnects and shows the question again, and Claude B is still
   waiting.
8. Exit Claude A: A's page greys out. Run `claude --resume` in A: the page
   redirects to the adopted workspace, with its history intact.
9. (M8) Run the skill on a change folder: the artifact sidebar lists proposal,
   ADRs, research and open questions; a question with a doc ref into an ADR
   highlights it.

## Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Claude paraphrases a quote | Highlights miss | Bridge pre-check with the same matcher returns an error naming the file and nearest headings; skill rule says "verbatim"; fuzzy stage; heading fallback |
| MCP tool timeout during a long wait | Claude sees an error | `AI_CONTEXTVIEW_ASK_WAIT` < `MCP_TOOL_TIMEOUT`; `pending` + `ask_wait` |
| Built-in question tool used anyway | Questions bypass the UI | `ai-contextview hook` blocks it while a spec is open; skill rule |
| Question arrives with no page open | User doesn't notice | `viewers: 0` → `ask` returns so Claude reopens the pane, else the bridge opens the URL; browser notification |
| Splitting goldmark text nodes is fiddly (ranges across inline nodes, entities) | Highlight lands wrong | Golden HTML tests from the quote fixtures; fall back to highlighting the containing block when a split isn't clean |
| Another local web page talks to the daemon | Forged answers | Loopback only; Host check (DNS rebinding); Origin check on POSTs; no CORS |
| Port 7878 taken | Daemon fails to start | Try the next 9 ports; URL comes back in `hello_ack` |
| `go/packages` slow on big modules | Slow `ask` | Per-package cache with mtime invalidation; load only the package containing the file, not `./...` |
| Atomic saves drop fsnotify watches | Spec stops updating | Watch the directory, not the file; also poll the mtime every 2s as a safety net |
| Bridge starts before the daemon and Claude times out the server start | MCP server marked failed | MCP stdio starts at once; the daemon connection is lazy and in the background |
| Old daemon still running after an upgrade | Protocol mismatch | Version in `hello`; clear error suggesting `ai-contextview serve --restart` |
| Hard to identify the Claude PID | Wrong label | Informational only; isolation never depends on it |

## Decisions taken

- **"Back to chat" copies the question to the clipboard** (text + options), so
  it can be pasted into the reply.
- **Non-Go code refs stay `lines` only.** Revisit after dogfooding.
- **Change folders use an OpenSpec custom schema** (M8).
- **Open items come from "Decide later"**, a choice per question, instead of a
  "keep as open question" checkbox.
- **The spec diff is a Changes tab** in the doc pane, against the last answers
  or the session start.
- **The built-in question tool is blocked per session by a hook**, never by a
  global deny rule.
- **The project is called ai-contextview** (v1.5.0). It shows each question
  with its context, and a spec is only one kind of working document. The
  README says that the "context" is the question's documents and code, not
  Claude's context window.
- **Every question offers Other.** The `ask` tool has no `allow_other`, so a
  free answer is always possible, as in the built-in question tool.
