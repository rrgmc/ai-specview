### Added

- Questions follow you to the chat when you are away from the machine, for
  example on Remote Control from another device: when a question waits 45
  seconds and nobody uses the viewer page, Claude asks it in the chat, until
  you use the viewer again. The page shows "Questions in chat" meanwhile.
  `/ai-contextview:ask-here` moves the questions to the chat at once, and
  `/ai-contextview:ask-here off` sends them back. `AI_CONTEXTVIEW_AWAY=wait`
  keeps the previous behavior.
