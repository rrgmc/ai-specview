# 04 — Claude Code integration

How a spec-elaboration skill uses ai-contextview.

## 1. Register the MCP server

The plugin in `plugin/` registers the MCP server, the skill and the hook in one
step (README, "Install"). Its second skill, `spec-elaborate-mod`, uses the pane
mod's own tools instead of the MCP server (06). The rest of this section
describes the manual setup.

Install the binary as the [README](../README.md#install) describes
(it must be on the `PATH` Claude Code sees), then register it per project in
`.mcp.json`, or user-wide with `claude mcp add --scope user ai-contextview -- ai-contextview mcp`:

```json
{
  "mcpServers": {
    "ai-contextview": {
      "command": "ai-contextview",
      "args": ["mcp"],
      "env": {
        "AI_CONTEXTVIEW_ASK_WAIT": "600s"
      }
    }
  }
}
```

Set the tool timeout in the shell profile or in Claude Code settings `env`:

```json
{ "env": { "MCP_TOOL_TIMEOUT": "1800000" } }
```

`AI_CONTEXTVIEW_ASK_WAIT` must be **lower** than `MCP_TOOL_TIMEOUT`, so a long wait
ends with a clean `pending` result instead of a timeout error.

## 2. Route all questions through ai-contextview

The skill and the `ask` tool description already tell Claude to ask every
question with `ask`. As a safety net, `ai-contextview hook` blocks the built-in
question tool (`AskUserQuestion`) **only while this Claude session has a spec
open in ai-contextview** (shown with `spec_show` and not finished with `spec_done`)
**or is in ask mode**, and tells Claude to use `ask` instead. Other sessions,
and spec-free work in the same session, keep the built-in tool. Turning ask
mode off (`ask_mode` with `off: true`) releases an open spec too: the hook
stops blocking until ask mode is on again or `spec_show` shows another spec
(01, `ask_mode`).

The same command also handles two more events. After Claude writes or edits a
markdown file (`PostToolUse` on `Write`, `Edit` and `MultiEdit`), it reports
the file, so that in ask mode the daemon can make it the working document. It
skips bookkeeping: `CLAUDE.md` and `AGENTS.md`, changelogs, Claude's memory,
and files in dot-folders other than `.claude/specs`. `AI_CONTEXTVIEW_DOC_PATHS`
(comma-separated globs) replaces these rules. When the file becomes the
working document, the hook tells Claude in one line (`additionalContext`).
When Claude starts a skill (`PreToolUse` on `Skill`), it reports the name, a
signal for spec mode. Before each of ai-contextview's own MCP tools, it reports
the session's folder, so a session in a desktop-app worktree resolves
relative paths against the worktree, not the main checkout (01). None of these
ever blocks.

**Spec mode** (`/ai-contextview:spec-mode [path or glob]`, the `spec-mode` skill)
is ask mode that expects a spec, for a spec written by any skill: the user's
own, a company's, or `spec-elaborate`. With a path or glob, the first written
file that matches becomes the working document; without one, the written
file that looks most like a spec (01, `ask_mode`; 07). The `spec-mode-mod`
skill does the same in the pane.

Ask mode routes every question of a session through ai-contextview without a
spec. The user turns it on at the start of a session with the `ask-mode`
skill (`/ai-contextview:ask-mode`), which calls the `ask_mode` tool (01); the
`ask-mode-mod` skill does the same in the pane (06). In ask mode refs are
optional, so Claude can ask about anything, and the hook's message to Claude
says so. `/ai-contextview:ask-mode off` turns it off.

Add it to the user settings (`~/.claude/settings.json`) once:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "^(AskUserQuestion|Skill|mcp__.*ai-contextview.*__.+)$",
        "hooks": [{ "type": "command", "command": "ai-contextview hook" }]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Write|Edit|MultiEdit",
        "hooks": [{ "type": "command", "command": "ai-contextview hook" }]
      }
    ]
  }
}
```

Use the absolute path of the binary (e.g. `/Users/me/go/bin/ai-contextview hook`,
or `C:/Users/me/go/bin/ai-contextview.exe hook` on Windows, where Claude Code runs
hooks with Git Bash) if `ai-contextview` is not on the PATH the desktop app sees.

How it matches: the bridge sends the daemon its parent process, which is the
Claude Code process of that session. The hook sends its own ancestor
processes, which include the same Claude Code process, so only that session is
affected. When the daemon is not running, is an older version, or anything
fails, the hook allows the tool. It answers within a second.

Do **not** deny `AskUserQuestion` globally in the settings: that also removes
it from every session that has nothing to do with ai-contextview.

## 3. Skill rules

Add a section like this to the spec-elaboration skill's `SKILL.md`. A complete
example skill lives in `plugin/skills/spec-elaborate/SKILL.md`.

```markdown
## Using ai-contextview

ai-contextview is a local viewer (a browser page) that shows the spec and code
beside the chat.

- At the start, as soon as the spec file path is known (created or existing),
  call `spec_show(path)`. Use the returned section names when you refer to
  sections later. Tell the user the viewer `url` from the result once. If the
  result has `open: "pane"` (the default) and the built-in browser tools are
  available, open `url` in the browser pane, unless the user asked for their
  own browser.
- After every change you make to the spec file, call
  `spec_show(path, changed_sections=[...])` with the headings you changed.
- Ask **every** clarifying question with the `ask` tool, never in plain chat
  text and never with another question tool. Group related questions into one
  call (max 6).
- Approvals go to chat: an answer in the viewer reaches Claude as a tool
  result, which Claude Code's permission checks (auto mode) do not take as the
  user's approval. So a question that asks the user to approve an action
  needing their permission is asked in plain chat. The `ask` tool
  description and the hook's block message say so too.
- For each question:
  - `spec_quote`: copy a short passage (one sentence or clause, under ~200
    characters) **verbatim** from the spec file. Don't paraphrase or fix typos.
  - `spec_section`: the heading of the section it's in.
  - `code_refs`: the code the question is about. For Go, prefer
    `symbol` (`pkg.Func`, `pkg.(*Type).Method`) over `lines`. Use `lines` for
    non-Go files or for a specific few lines inside a big function.
  - If the question is about a gap (something the spec doesn't cover yet),
    quote the nearest existing sentence and set `spec_section`.
  - `doc_refs`: when the question also depends on another markdown document
    (an ADR, a research note, another spec), add `{path, quote, section}` for
    it, with the same verbatim rule.
- If `ask` returns an error, fix exactly what it says (quote not found, unknown
  symbol) and call it again. Don't fall back to asking in chat because of
  validation errors.
- Results:
  - `answered`: apply the answers to the spec.
  - `in_chat`: stop and wait for the user's message. The question text is
    already on the user's clipboard.
  - `pending`: call `ask_wait` with the returned id.
  - `ask_in_chat`: the user is away from the viewer; ask the same questions
    with the built-in question tool, which the hook allows then (01,
    `ask_here`). The `ask-here` skill moves the questions to the chat on
    request, for a user who stopped a waiting `ask` from another device.
  - `dismissed`: continue without that decision, or ask in chat if it's
    essential.
  - `keep_open` (with `in_chat` or `dismissed`): in a change folder, write one
    `open-questions/<slug>.md` per listed question (the question, its options,
    and the doc and code refs). Remove the file once it is resolved later.
- If a tool says the ai-contextview viewer is not reachable, continue normally and
  ask in chat.
- Use `focus` to point the user at a spec part and code without asking anything,
  e.g. when summarizing a change.
- At the very end, call `spec_done(summary)` once. The summary is markdown
  with a **Decided** and a **Still open** heading, each followed by a bullet
  list, one item per decision or open point. The viewer shows a "Spec
  finished" banner. A later `ask` reopens the session.
```

## 4. Optional hooks

Besides `ai-contextview hook` (section 2), these aren't needed, because the daemon
watches files, but they're useful later:

- **`SessionStart`** hook: send `{session_id, cwd}` to the daemon (via a tiny
  `ai-contextview notify` subcommand) so the tab can show Claude's own session ID
  and link to the transcript. The workspace is matched by `cwd` + `claude_pid`.
- **`PostToolUse`** on `Write|Edit`: if the edited path is the current spec,
  notify the daemon with the tool's input, which gives exact edited ranges for
  sharper "changed" marks. Only worth it if hash-based section diffing turns out
  to be too coarse.

## 5. Example skill

`plugin/skills/spec-elaborate/` (built in M6):
- `SKILL.md`: the workflow (understand the request → read the relevant code →
  draft the spec → ask in rounds → refine → finish) plus the rules above.
- `templates/spec.md`: the section skeleton (Overview, Goals, Non-goals, Design,
  API, Data, Errors, Rollout, Open questions, Decisions).
- The workflow records decisions: each `answered` result is appended under
  **Decisions** in the spec, which also marks that section as changed in
  ai-contextview. (In M8 this changes: decisions become ADR files and unanswered
  questions become open-question files in a change folder; see 00 "Future".)
