---
name: ask-here
description: Move ai-contextview's questions from the viewer (a browser page on the machine running Claude) to the chat, for a user who is away from that machine, for example following the session over Remote Control from another device. Use when the user runs /ask-here, or says they cannot see or reach the viewer, are on their phone or another computer, or want to answer here in the chat for now; "/ask-here off" sends the questions back to the viewer. Do NOT use to turn ask mode off for good (use ask-mode with off).
argument-hint: "[off]"
---

# Ask in the chat while away from the viewer

The ai-contextview viewer is a page on the machine running Claude, so a user
following the session from another device cannot open it. This moves the
questions to the chat until the user is back at the viewer. Ask mode and an
open spec stay as they are.

Use the viewer's tool `mcp__plugin_ai-contextview_ai-contextview__ask_here`
(load it with ToolSearch if it is deferred), never the `mcp__ai-contextview__…`
tools, which drive the pane.

- With no argument, call `ask_here` with no input. If the result has
  `closed`, or a question was waiting in the viewer when the user stopped
  you, ask those questions again now with the built-in question tool. Tell
  the user in one line that questions come to the chat until they use the
  viewer again.
- With the argument `off`, call `ask_here` with `{"off": true}` and tell the
  user in one line that questions go to the viewer again.

While the questions are in the chat, keep calling `ask` as the active skill
says: it returns `ask_in_chat` at once, and then you ask the same questions
with the built-in question tool. Once the user uses a viewer page again,
`ask` shows the questions there again by itself.
