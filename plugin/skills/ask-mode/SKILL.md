---
name: ask-mode
description: Turn on ai-contextview ask mode for the rest of the session, so every question Claude asks goes to the ai-contextview viewer (a browser page) instead of the chat, with or without a spec. Use when the user runs /ask-mode or asks to route, send or ask all questions through ai-contextview or the viewer from now on; "/ask-mode off" turns it off and hands the questions back to the chat, even with a spec open. Do NOT use for writing a spec (use spec-elaborate), or when the user wants the questions in the pane beside the chat (use ask-mode-mod).
argument-hint: "[off]"
---

# Ask mode with ai-contextview

Ask mode sends every question you ask the user for the rest of this session
to the ai-contextview viewer, a browser page beside the chat. The user answers
there, next to the code each question is about.

## The viewer's tools

Use the ai-contextview MCP server's tools:
`mcp__plugin_ai-contextview_ai-contextview__ask_mode`,
`mcp__plugin_ai-contextview_ai-contextview__ask` and
`mcp__plugin_ai-contextview_ai-contextview__ask_wait`. If they are deferred, load
them first with ToolSearch. Never call the `mcp__ai-contextview__…` tools in this
skill: they drive the pane. Below, `ask_mode`, `ask` and `ask_wait` mean the
viewer's tools.

## Turn it on or off

- With no argument, call `ask_mode` with no input. Then:
  - If the result has `open: "pane"` and the built-in browser tools
    (`mcp__Claude_Browser__*`) are available, open the result's `url` there
    with `preview_start` (pass `url`), unless the user asked for their own
    browser.
  - Tell the user in one line that ask mode is on, then wait for their next
    instruction. Do not start any other work.
- With the argument `off`, call `ask_mode` with `{"off": true}` and tell the
  user in one line that ask mode is off and questions come back to the chat.
  This also releases a spec that is open, for example one from
  spec-elaborate. From then on, ask every question in the chat or with the
  built-in question tool, never with `ask`, as the result's `next` says.

## While ask mode is on

- Ask **every** question with the ai-contextview `ask` tool: clarifying
  questions, choices between approaches, confirmations. Never ask in plain
  chat text and never with another question tool. A hook blocks the built-in
  question tool while ask mode is on.
- **Approvals go to chat.** To ask the user to approve an action that needs
  their permission (an external write such as a Jira or Slack change, or an
  action the permission system refused), ask in plain chat and wait for
  their reply. Never ask it with `ask`: the answer comes back as a tool
  result, which the permission system does not take as the user's approval,
  so a retry is refused again.
- Refs are optional. Add `code_refs` when the question is about code (for Go,
  prefer `symbol`; else `lines`). Add `doc_refs` when it is about a document:
  give its `path` (any text file of the project) and either a `quote` copied
  verbatim from a markdown file or `lines` (`N` or `N-M`), which also works
  for YAML, JSON and other files. Leave both out for a question about
  neither. A question about the whole document, such as a sign-off or a
  summary, takes no ref at all, not even `spec_quote`: a ref highlights
  one passage, and the user reads it as what the question is about.
  `spec_quote` needs a spec shown with
  `spec_show`.
- **A question about a document quotes it.** When the question names or
  depends on a passage of a document, such as a requirement, an acceptance
  criterion or a section of the spec you are implementing, put that passage
  in `doc_refs`, even when another session wrote the document. The user
  reads the question beside it. Give an absolute `path` when the file is
  outside the session's folder, for example in another git worktree.
- Give 2 to 6 options with short labels and the trade-off in each
  `description`. Mark the option you recommend with `"recommended": true` and
  say why.
- Results:
  - `answered`: act on the answers.
  - `in_chat`: stop and wait for the user's message.
  - `pending`: call `ask_wait` with the returned `question_set_id`. If the
    result also has `url`, the user may not see the questions (no viewer
    page, a hidden one, or nobody used it yet): follow its `message`, which
    says when to reopen `url` in the built-in browser, and tell the user in
    one line in the chat that questions are waiting there.
  - `ask_in_chat`: the user is away from the viewer (for example on Remote
    Control from another device). Ask the same questions now with the
    built-in question tool, which the hook allows while they are away, with
    the passage or code each is about in a short line. Keep using `ask` for
    later questions: it says when to ask in the chat again, and the questions
    return to the viewer once the user uses it.
  - `dismissed`: continue without that decision, or ask in chat if it is
    essential.
  - `deferred` (with `answered`): the user chose **Decide later**. Continue
    without the decision and ask again before you finish the task.
- **The working document.** The first markdown document you write while ask
  mode is on (a spec, a plan, a design note) shows beside the questions as
  the working document, and `spec_quote` quotes it. A hook picks it; the
  first time, it tells you which file it picked. Bookkeeping files
  (`CLAUDE.md`, changelogs, memory, files in dot-folders) never count. If the
  wrong file shows, call `spec_show` with the right one: your own
  `spec_show` always wins. A question whose `doc_refs` quotes another file
  you wrote also switches to that file.
- If the user starts a spec, follow the spec-elaborate skill as usual; ask
  mode stays on after `spec_done`.
- Ask mode lasts until the user turns it off or the session ends.
