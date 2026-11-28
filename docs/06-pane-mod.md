# 06 — Pane mod

The `ai-contextview` plugin has two ways to show a spec:

| Skill | Tools | Viewer |
|---|---|---|
| `ai-contextview:spec-elaborate` | The bridge's MCP tools, `mcp__plugin_ai-contextview_ai-contextview__…` | The browser page served by the daemon |
| `ai-contextview:spec-elaborate-mod` | The mod's own tools, `mcp__ai-contextview__…` | A pane beside the chat, drawn by a Claude Code mod |

Ask mode (04) has a skill for each: `ai-contextview:ask-mode` uses the bridge's
`ask_mode` tool, `ai-contextview:ask-mode-mod` the mod's. The mod keeps the mode
in its state, blocks `AskUserQuestion` while it is on, accepts questions with
no refs, and with no spec shown draws the pane as "Ask mode" without a spec
file or section list. A `spec_quote` still needs a spec.

Doc refs follow the bridge's rules (01): any text file of the project, by
`quote` (markdown) or `lines`. A file that is not markdown shows as code: the
lines before the range, the range in the framed box, and the lines after.
With no spec, the document column shows for a whole question set when any of
its questions has a document, and is left out otherwise; a question with no
document keeps the last one and says so. `AI_CONTEXTVIEW_ASK_MODE=always` turns
ask mode on when the session starts.

The mod picks the working document from written files as the daemon does (01,
`ask_mode`; 07): its `tool.call` hook sees `Write`, `Edit` and `MultiEdit`
after they run, applies the hook's file rules and `AI_CONTEXTVIEW_DOC_PATHS`, and
shows the picked file as `spec_show` would; a `Skill` call raises the spec-mode
score, and a spec-mode path that names an existing markdown file shows at
once, found in another git worktree as the bridge finds it. The pick lives in the mod's state (`work`). Instead of a line to Claude,
it shows a toast to the user.

The mod is `plugin/hooks/register.tsx`, listed under `modules` in
`plugin/hooks/hooks.json` beside the `PreToolUse` hook. It does not use the
bridge or the daemon. Both skills run the same workflow; the user picks one by
asking for the pane or the browser.

## 1. Why a mod

A mod is a plugin's TypeScript hooks module that Claude Code runs in its own
process. It can register tools, hook tool calls, keep session state and draw
panes with Claude Code's own elements (`Box`, `Text`, `Markdown`, `Code`,
`Select`, `Button`, `Input`). The pane sits in the Claude window, so the person
does not switch to a browser tab.

What a mod cannot do, and the browser page can:

- Colour a span inside rendered markdown. `Markdown` and `Code` take whole
  text, with no ranges.
- Reuse the page's HTML, CSS and JavaScript.
- Draw more than about 100,000 characters at once.
- Show the workspaces of several Claude sessions in one place.

## 2. The tools

At `session.start` the mod registers `spec_show`, `focus`, `ask`, `ask_wait`
and `spec_done` (`plugin/hooks/tools.ts`), with the bridge's input schemas
(`internal/protocol/mcp.go`) and descriptions that name the pane. Claude Code
lists them as `mcp__ai-contextview__<name>`, behind ToolSearch like MCP tools. A
`tool.call` hook answers them, so they need no permission prompt. Each answer
is the JSON text the bridge would send: an MCP tool's result must be text, not
an object.

- **Errors.** Bad input or a ref that is not found fails the call with the
  bridge's wording ("invalid input" or "fix these and call again"). If a hook
  fails or the person interrupts the call, the hook's `.catch` refuses it.
- **`AskUserQuestion`.** `ai-contextview hook` asks the daemon whether a spec is
  open, and the pane does not use the daemon. So the mod blocks the built-in
  question tool itself, with the hook's message, from `spec_show` to
  `spec_done`. Turning ask mode off releases the spec, as in the daemon:
  the block ends until ask mode is on again or another spec is shown.
- **`/contextview-pane`** opens the pane again after the person closed it.

## 3. Specs, doc refs and files

`spec_show` loads a spec as the bridge's `specsrc.LoadRoot` does: a markdown
file, or a folder whose markdown files are listed up to 3 levels deep, dot
entries skipped. In a folder the primary file is `primary`, else
`proposal.md`, `spec.md`, `README.md`, else the first file. `changed` names
other files of a folder that changed. The result lists each file as an
artifact; the kind is always `other`, since the mod does not infer kinds.

`doc_refs` in `focus` and in each question resolve as the bridge's
`resolveDocs`: a path relative to the folder of a folder spec, else to the
project directory; no path means the primary file. The file must be markdown,
inside the folder for a folder spec, and hold the quote.

## 4. Questions

- **Input.** `ask` checks its input with the rules of `internal/validate`
  (`plugin/hooks/validate.ts`: 1-6 questions, 2-6 options, at most one
  recommended, refs required, quote length) and turns a "(Recommended)" label
  suffix into the flag. Each question's quote, doc refs and code refs are
  resolved as `focus` resolves them.
- **Waiting.** The question set is stored in session state and the pane shows
  it. The `tool.call` hook then waits for the person's choice, checking once a
  second, for up to 10 minutes (the bridge's `AI_CONTEXTVIEW_ASK_WAIT` default).
  Past that it answers `pending` with the question set id, and `ask_wait` waits
  again. A hook's own time is capped at 10 seconds and a `$.clock` wait counts
  toward it, while time inside any other `$` call does not (up to 20 minutes on
  `tool.call`). So the hook pauses between checks with `/bin/sleep 1` through
  `$.process.run`. A hidden tool of the mod's own does not lift the cap: its
  `tool.run` hook is held to the same 10 seconds.
- **Answers.** The form's buttons store the outcome in session state and in a
  map the waiting hook reads. `ask_wait` reads session state too, so an answer
  given while the mod reloaded is not lost. The result has the bridge's shape
  and messages: `answered` with `answers` (and `deferred` for "Decide later"),
  `in_chat` for "Back to chat", which also copies the questions to the
  clipboard, and `dismissed` for "Skip". "Send answers" names the questions
  still unanswered and goes to the first. There is no `keep_open`.
- **No move to the chat.** The bridge moves unseen questions to the chat for
  a user on another device (`ask_here`, 01). The mod cannot tell whether
  anyone sees the pane, so it has no `ask_in_chat` and no `ask_here`; a user
  away from the machine stops the waiting `ask` and runs
  `/ai-contextview:ask-mode-mod off`.
- **Review.** A single choice or "Decide later" moves on to the next
  unanswered question. Once every question has an answer, the form shows a
  review in its place, as the browser page does: one line per question with
  its answer and notes. A line, or a question number, goes back to that
  question; "Review" on the last question returns to the review.
- **`spec_done`.** Checks the summary's length (8,000 bytes), clears the focus
  and shows a "Spec finished" banner with the summary. The next `spec_show` or
  `ask` reopens the spec.

## 5. Matching without the bridge

`plugin/hooks/spec.ts` is the subset of `internal/specsrc` and
`internal/resolve` the pane needs:

- Headings, with inline code marks dropped as the bridge drops them.
- Quotes: exact, then with runs of whitespace collapsed. No fuzzy stage and no
  heading fallback.
- Code refs: `N` or `N-M` line ranges, and Go symbols found by a regular
  expression over the file (`func`, method receiver, `type`, `var`, `const`),
  doc comment included. A symbol in another file of the package is not found.

## 6. The pane

| Part | What it does |
|---|---|
| Header | One line: the file shown, "› " and the section shown, the spec it belongs to when that is another file, the quote match |
| File picker | `Select` over the spec's files and any other file a doc ref quotes, marked "(changed)" and "(quoted)"; shown when there is more than one |
| Section picker | `Select` over "Whole spec" (or "Whole file") and every heading of the file shown, indented by level; changed sections are marked |
| Jump links | A row under the pickers, shown when it has a link: one per changed section; "Quote in …" per doc ref in another file; "Back to the focus" or "Back to the question" after the person moved away |
| Spec column | The section shown, as markdown. The block that holds the quote is framed in yellow, with the quote in bold. Its caption is "Focus", or "Question N:" and the question on one line |
| Side column | The question form while questions wait, and one `Button` tab per code ref with the `Code` of the active one, with line numbers |
| Question form | The question, its options (a recommended one marked in green, each description under its button), "Decide later" (left out when an option already offers it, 01-protocol.md), Other and Notes. Under them a row of Previous, Next or Review, then a row of "Send answers", primary once every question has an answer, and the plain links "Back to chat" and "Skip" |
| Banner | After `spec_done`, "Spec finished" with the summary |

- **Columns.** Side by side (55% and 45%) when the pane is docked and at least
  100 cells wide, and there is a question or a code ref to show; with neither,
  the spec takes the whole width. Otherwise the question form comes first,
  with the quote repeated under the question, then the spec, then the code.
- **Side tabs.** In two columns, while questions wait and the current question
  has code refs, a "Question" tab ("Answers" in the review) leads the code
  tabs, and the side column shows either the form or one code ref. Each
  question opens on the form; `codeTab` is -1 while it shows.
- **Where it opens.** A focus or a question opens on the primary quote's
  section, else on the first doc ref's file and section. Moving between
  questions moves the pane with them, and the pane scrolls the frame into view.
- **Focus block.** `splitAround` cuts the section into before, focus and after
  at blank lines outside code fences, so each part is whole markdown.
- **State.** `view` holds the spec, its files and the last focus; `ui` holds
  the file and section shown and the code tab; `ask` holds the question set,
  the drafts and the outcome. All are session state with a shape tag, so a
  reload keeps them and a changed shape discards the old value. A file other
  than the primary is read when the pane draws.

## 7. Tests

`claude plugin test ./plugin` runs `plugin/tests/pane.test.tsx` against
Claude Code's test kit. It covers the layout at two widths, the section and
file pickers, code tabs, doc refs in a file and in a folder spec, the error
paths, answering, "Decide later", "Back to chat", "Skip", the `pending` timeout
and `ask_wait`, input checks, the `AskUserQuestion` block and the `spec_done`
banner. The tests use `mock.clock`, which holds each pause until the test
moves the clock. The test kit does not check an MCP tool's result shape, and
its clock costs no real time, so the result format and the time cap were
found in live runs.
