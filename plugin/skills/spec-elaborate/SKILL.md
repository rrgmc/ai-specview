---
name: spec-elaborate
description: Elaborate a feature spec with the user, showing the spec and the code side by side in the ai-contextview viewer. Use when asked to "spec", "write a spec for", "elaborate the spec", or "design" a change before writing code. Asks every clarifying question through the ai-contextview `ask` tool, with the spec passage and the code it is about. Do NOT use for writing the code itself, or when the user wants the spec in a pane beside the chat (use spec-elaborate-mod).
---

# Spec elaboration with ai-contextview

You write or refine a markdown spec together with the user. ai-contextview is a
local viewer (a browser page) that shows the spec and the code beside the chat.
The user sees each question next to the spec passage and the code it is about.

## Workflow

1. **Understand the request.** Restate the goal in one or two sentences. Find
   the spec file: an existing one the user names, or a new one created from
   `templates/spec.md`. A new spec is a draft outside the repository (see
   **Draft specs**), unless the user asks for it in the repository.
2. **Show it.** As soon as the file exists, call `spec_show(path)`. Tell the
   user the viewer `url` from the result, once. Use the returned section names
   when you refer to sections. Open the viewer in the built-in browser when
   the result has `open: "pane"` (see **Built-in browser**).
3. **Read the relevant code.** Find the packages, types and functions the
   change touches. Note the exact symbols; you will cite them in questions.
4. **Draft.** Fill in the sections you can from the request and the code. Put
   every open point in **Open questions**. After the edit, call
   `spec_show(path, changed_sections=[...])` with the headings you changed.
5. **Ask in rounds.** Group related open points into one `ask` call (at most 6
   questions). Follow the rules below.
6. **Refine.** Apply each answer to the spec, record it under **Decisions**,
   remove it from **Open questions**, and call `spec_show` with the changed
   headings. Ask the next round.
7. **Finish** when no open questions remain or the user says so. Summarize the
   spec in a few lines in the chat. Then call `spec_done(summary)` once. The
   summary is markdown in this shape, one bullet per item, never a run-on
   paragraph:

   ```markdown
   **Decided**

   - Retries keep growing on partial success.
   - The cap resets daily at midnight UTC.

   **Still open**

   - Jitter range (needs a benchmark).
   ```

   The viewer shows it as a "Spec finished" banner and clears the highlights
   and the code pane, so do not call `focus` first. If the user asks for more
   changes later, just continue; the next `ask` reopens the session.
   For a draft spec, ask whether to move it into the repository before
   calling `spec_done` (see **Draft specs**).

## Built-in browser

The Claude desktop app has a built-in browser (its tools are named
`mcp__Claude_Browser__*`). Open the viewer there, once per session, when
the `spec_show` result has `open: "pane"` (the default), unless the user asked
for their own browser. Also open it there when the user asks for the built-in
browser and `open` is missing.

- If the built-in browser tools are available, open the `url` from the
  `spec_show` result with `preview_start` (pass `url`). If the user must allow
  the site first, say so in one line.
- If they are not available (the CLI, an IDE), do nothing. The bridge opens
  the default browser when an `ask` finds no open page.
- Do not use that browser tab for other work during the session, so the
  viewer stays on screen. Open another tab if you need one.
- Open it again when the user asks, or when an `ask` result is `pending`
  with a `url` and its `message` says to: no page is open, or it is hidden.

## Draft specs

A new spec is a draft by default. A draft lives outside the repository, like
a plan in plan mode, so it needs no branch and is never committed by accident.
Write the new spec in the repository only when the user asks for it, for
example "in the repo", "in docs/specs" or a path in the repository. Then use
`docs/specs/<slug>.md`, or the path the user names, and skip the rest of this
section. An existing spec the user names stays where it is.

- Put the draft at `~/.claude/specs/<project>/<slug>.md`. Write the path with
  the `~` as it is, in the file tools (Read, Write, Edit) and in `spec_show`
  and the other tools: all of them expand a leading `~/` to the user's home
  folder, on macOS, Linux and Windows. Do not expand it in the shell. Code
  refs stay relative to the current folder, as usual.
- `<project>` is the name of the main checkout's folder, so every worktree of
  a repository shares one folder. Run
  `git rev-parse --path-format=absolute --git-common-dir`, which works the
  same in Bash, Git Bash and PowerShell, and take the name of the folder that
  holds the `.git` it prints: for `/home/me/src/shop/.git` it is `shop`.
  Outside a git repository, use the name of the current folder.
- Tell the user the path once, when you create the file.
- Before `spec_done`, ask with `ask` whether to keep the draft or move it to
  `docs/specs/<slug>.md` in the repository. Quote the spec's title line. If
  the user moves it, move the file (to the path the user names, if any), call
  `spec_show` with the new path, and do not commit it unless the user asks.
  Then call `spec_done`.

## Using ai-contextview

- Ask **every** clarifying question with the `ask` tool, never in plain chat
  text and never with another question tool.
- If the user turns ai-contextview off (`ask_mode` with `off: true`, from
  the ask-mode or spec-mode skill), ask every later question in the chat
  instead, and keep the spec and `spec_show` as before.
- **Approvals go to chat.** To ask the user to approve an action that needs
  their permission (an external write such as a Jira or Slack change, or an
  action the permission system refused), ask in plain chat and wait for
  their reply. Never ask it with `ask`: the answer comes back as a tool
  result, which the permission system does not take as the user's approval,
  so a retry is refused again.
- For each question:
  - `spec_quote`: copy a short passage (one sentence or clause, under about 200
    characters) **verbatim** from the spec file. Do not paraphrase or fix typos.
  - `spec_section`: the heading of the section it is in.
  - `code_refs`: the code the question is about. For Go, prefer `symbol`
    (`pkg.Func`, `pkg.(*Type).Method`) over `lines`. Use `lines` (`N-M`) for
    other files, or for a few lines inside a big function.
  - If the question is about a gap the spec does not cover yet, quote the
    nearest existing sentence and set `spec_section`.
  - `doc_refs`: when the question also depends on another document (an ADR,
    a research note, another spec, a config file), add `{path, quote,
    section}` with the same verbatim rule, or `{path, lines}` (`N` or `N-M`)
    for a file that is not markdown.
  - Give 2 to 6 options with short labels. Put the trade-off in each option's
    `description`.
  - Mark the option you recommend with `"recommended": true` (at most one per
    question), and say why in its description.
- If `ask` returns an error, fix exactly what it says (quote not found, unknown
  symbol) and call it again. Do not fall back to chat because of a validation
  error.
- Results:
  - `answered`: apply the answers to the spec.
  - `in_chat`: stop and wait for the user's message. The questions are already
    on the user's clipboard.
  - `pending`: call `ask_wait` with the returned `question_set_id`. If the
    result also has `url`, the user may not see the questions (no viewer
    page, a hidden one, or nobody used it yet): follow its `message`, which
    says when to reopen `url` in the built-in browser (see **Built-in
    browser**), and tell the user in one line in the chat that new questions
    are waiting there.
  - `ask_in_chat`: the user is away from the viewer (for example on Remote
    Control from another device). Ask the same questions now with the
    built-in question tool, which the hook allows while they are away, with
    the passage or code each is about in a short line. Keep using `ask` for
    later questions: it says when to ask in the chat again, and the questions
    return to the viewer once the user uses it.
  - `dismissed` (the user clicked Skip): continue without that decision, or
    ask in chat if it is essential.
  - `deferred` (with `answered`): the user chose **Decide later** for these
    questions. Add each to **Open questions** in the spec (in a change folder,
    one `open-questions/<slug>.md` file) with the question, its options, any
    notes and its refs. Continue without the decision. Before finishing, ask
    the deferred questions again in one `ask`; whatever is still deferred goes
    under "Still open" in the `spec_done` summary.
  - `keep_open` (with `in_chat` or `dismissed`): in a change folder, write one
    `open-questions/<slug>.md` per listed question, with the question, its
    options and its refs. Remove the file once the question is resolved.
- If a tool says the ai-contextview viewer is not reachable, continue normally and
  ask in chat.
- Call `spec_done` only at the very end, never between rounds.
- Use `focus` to point the user at a spec passage and code without asking
  anything, for example when summarizing a change.

## Recording decisions

Append each answered question under **Decisions** as one bullet:
`- <decision> (<short reason>; asked <date>)`. Keep the wording short; the
spec is the plan the next agent implements.
