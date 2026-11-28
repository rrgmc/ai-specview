package protocol

// MCP tool inputs and outputs. The go-sdk derives the JSON Schemas from these
// structs, so there is no hand-written schema.

// SpecShowInput is the input of the spec_show tool.
type SpecShowInput struct {
	Path            string        `json:"path" jsonschema:"a .md file or a folder of markdown artifacts, relative to the project directory or absolute"`
	Primary         string        `json:"primary,omitempty" jsonschema:"folders only: the file shown first and used by refs without a path"`
	ChangedSections []string      `json:"changed_sections,omitempty" jsonschema:"headings you just changed in the primary file"`
	Changed         []ChangedFile `json:"changed,omitempty" jsonschema:"folders only: other files you just created or edited"`
	Provisional     bool          `json:"provisional,omitempty" jsonschema:"ask mode or spec mode only: show this file (for example exploration notes) until the working document is written; the first written file that would become the working document replaces it"`
}

// SpecShowOutput is the output of the spec_show tool.
type SpecShowOutput struct {
	OK        bool       `json:"ok"`
	URL       string     `json:"url,omitempty" jsonschema:"the viewer page for this session"`
	Open      string     `json:"open,omitempty" jsonschema:"where the user wants the viewer: pane means open url in the built-in browser pane when one is available"`
	Primary   string     `json:"primary"`
	Sections  []string   `json:"sections" jsonschema:"headings of the primary file"`
	Artifacts []Artifact `json:"artifacts"`
}

// FocusInput is the input of the focus tool.
type FocusInput struct {
	SpecQuote   string    `json:"spec_quote,omitempty" jsonschema:"verbatim passage of the primary spec file"`
	SpecSection string    `json:"spec_section,omitempty" jsonschema:"heading of the section containing spec_quote"`
	DocRefs     []DocRef  `json:"doc_refs,omitempty" jsonschema:"passages of other markdown artifacts"`
	CodeRefs    []CodeRef `json:"code_refs,omitempty" jsonschema:"code to open; each needs path and exactly one of symbol or lines"`
}

// FocusOutput is the output of the focus tool.
type FocusOutput struct {
	OK        bool          `json:"ok"`
	SpecMatch Match         `json:"spec_match,omitempty"`
	Docs      []DocMatch    `json:"docs"`
	Code      []ResolvedRef `json:"code"`
}

// QuestionInput is one question of the ask tool.
type QuestionInput struct {
	ID          string    `json:"id" jsonschema:"short unique id, e.g. q1"`
	Text        string    `json:"text" jsonschema:"the question"`
	Options     []Option  `json:"options" jsonschema:"2-6 options"`
	MultiSelect bool      `json:"multi_select,omitempty" jsonschema:"allow several options"`
	SpecQuote   string    `json:"spec_quote,omitempty" jsonschema:"verbatim passage (3-300 characters) of the primary spec file this question is about"`
	SpecSection string    `json:"spec_section,omitempty" jsonschema:"heading of the section containing spec_quote"`
	DocRefs     []DocRef  `json:"doc_refs,omitempty" jsonschema:"up to 4 passages of other markdown artifacts"`
	CodeRefs    []CodeRef `json:"code_refs,omitempty" jsonschema:"up to 4 code refs; each needs path and exactly one of symbol or lines"`
}

// AskInput is the input of the ask tool.
type AskInput struct {
	Title     string          `json:"title,omitempty" jsonschema:"short title of the question set"`
	Questions []QuestionInput `json:"questions" jsonschema:"1-6 questions answered together"`
}

// AskOutput is the output of ask and ask_wait.
type AskOutput struct {
	Status        string   `json:"status" jsonschema:"answered, in_chat, ask_in_chat, pending or dismissed"`
	Answers       []Answer `json:"answers,omitempty"`
	Deferred      []string `json:"deferred,omitempty" jsonschema:"questions the user chose to decide later; record them as open questions and ask again before finishing"`
	KeepOpen      []string `json:"keep_open,omitempty" jsonschema:"questions the user wants kept as open questions"`
	QuestionSetID string   `json:"question_set_id,omitempty"`
	// URL is set on a pending result when no page shows the questions.
	URL     string `json:"url,omitempty" jsonschema:"set when no viewer page is open: open this url in the built-in browser pane, then call ask_wait"`
	Message string `json:"message,omitempty"`
}

// SpecDoneInput is the input of spec_done.
type SpecDoneInput struct {
	Summary string `json:"summary,omitempty" jsonschema:"summary of the finished spec as markdown with a **Decided** heading and a **Still open** heading, each followed by a bullet list with one item per decision or open point (never a run-on paragraph)"`
}

// SpecDoneOutput is the output of spec_done.
type SpecDoneOutput struct {
	OK  bool   `json:"ok"`
	URL string `json:"url,omitempty"`
}

// AskModeInput is the input of ask_mode.
type AskModeInput struct {
	Off   bool     `json:"off,omitempty" jsonschema:"true turns ask mode off and hands the questions back to the chat, even with a spec open; omit to turn it on"`
	Spec  bool     `json:"spec,omitempty" jsonschema:"spec mode: ask mode that expects a spec and shows it as the working document once it is written"`
	Paths []string `json:"paths,omitempty" jsonschema:"spec mode: glob patterns of the spec file, relative to the project dir or absolute (** for any folders); the first written file that matches is shown; a plain path to an existing markdown file is shown at once, looked up in the repository's other git worktrees when it is not in the session's folder"`
}

// AskModeOutput is the output of ask_mode.
type AskModeOutput struct {
	On   bool   `json:"on"`
	Spec bool   `json:"spec,omitempty"`
	URL  string `json:"url,omitempty" jsonschema:"the viewer page for this session"`
	Open string `json:"open,omitempty" jsonschema:"where the user wants the viewer: pane means open url in the built-in browser pane when one is available"`
	// Shown is a spec-mode path that already existed and now shows as the
	// working document.
	Shown string `json:"shown,omitempty" jsonschema:"spec mode: the existing spec file now shown as the working document; spec_quote quotes it"`
	// Next, when ask mode was turned off, tells Claude to ask in the chat
	// again: the skills loaded earlier in the session say otherwise.
	Next string `json:"next,omitempty"`
}

// OffNext is AskModeOutput.Next when ask mode is turned off.
const OffNext = "ai-contextview is off for this session. Ask every question in the chat or with the built-in question tool from now on, not with the ask tool, even while a spec is open. The spec stays in the viewer."

// AskHereInput is the input of ask_here.
type AskHereInput struct {
	Off bool `json:"off,omitempty" jsonschema:"true moves the questions back to the viewer; omit to ask in the chat"`
}

// AskHereOutput is the output of ask_here.
type AskHereOutput struct {
	InChat bool `json:"in_chat"`
	// Closed is the question set that was waiting in the viewer, if any.
	Closed  string `json:"closed,omitempty" jsonschema:"the question set that was waiting in the viewer: ask its questions in the chat now"`
	Message string `json:"message"`
}

// AskWaitInput is the input of ask_wait.
type AskWaitInput struct {
	QuestionSetID string `json:"question_set_id" jsonschema:"the id returned by a pending ask"`
}
