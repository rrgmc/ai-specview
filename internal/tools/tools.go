// Package tools implements the MCP tools of `ai-contextview mcp`: spec_show, focus,
// ask, ask_wait, ask_mode and spec_done.
package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rrgmc/ai-contextview/internal/gitwt"
	"github.com/rrgmc/ai-contextview/internal/identity"
	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/resolve"
	"github.com/rrgmc/ai-contextview/internal/specsrc"
	"github.com/rrgmc/ai-contextview/internal/validate"
)

// Daemon is the bridge's view of the daemon connection (appconn.Client).
type Daemon interface {
	Request(ctx context.Context, typ string, body any) (protocol.Envelope, error)
	WaitResult(ctx context.Context, qs string, d time.Duration) (*protocol.AskResult, error)
	HasResult(qs string) bool
	URL() string
}

// Bridge holds the per-instance state of the tool handlers.
type Bridge struct {
	Cwd      string
	Daemon   Daemon
	Resolver resolve.Resolver
	// AskWait is how long ask and ask_wait block before returning pending.
	AskWait time.Duration
	// Open opens a URL in the browser; nil never opens.
	Open func(url string)
	// OpenIn is reported in the spec_show result: OpenPane asks Claude to open
	// the viewer in the built-in browser pane (see OpenInFromEnv).
	OpenIn string
	// ReopenDelay is how long ask waits, in pane mode, for a page to connect
	// before it opens the system browser instead. Zero means DefaultReopenDelay.
	ReopenDelay time.Duration
	// NudgeDelay is how long ask waits before it returns pending, so that
	// Claude says in the chat that questions are waiting, when nobody has
	// used the viewer page since the questions were shown. Zero means
	// DefaultNudgeDelay.
	NudgeDelay time.Duration
	// StayInViewer keeps unseen questions in the viewer (pending and a line
	// in the chat) instead of moving them to the chat (see AwayFromEnv).
	StayInViewer bool
	Logf         func(format string, args ...any)

	mu      sync.Mutex
	root    *specsrc.Root
	pending map[string]bool
	askMode bool
	// rootAuto is set while root is a document the daemon picked from a
	// write (doc_shown), not one shown with spec_show.
	rootAuto bool
	// sessionCwd is the Claude session's folder when the daemon reported one
	// (session_cwd); it replaces Cwd for relative paths.
	sessionCwd string
}

// cwd is the folder relative paths resolve against: the session's folder
// when the daemon reported it (a desktop-app worktree), else Cwd.
func (b *Bridge) cwd() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionCwd != "" {
		return b.sessionCwd
	}
	return b.Cwd
}

// SessionCwd follows the daemon when the hook reported the session's folder.
func (b *Bridge) SessionCwd(s protocol.SessionCwd) {
	if !filepath.IsAbs(s.Cwd) {
		return
	}
	b.mu.Lock()
	b.sessionCwd = filepath.Clean(s.Cwd)
	b.mu.Unlock()
}

// Messages returned to Claude.
const (
	MsgInChat  = "The user will answer in the chat. Wait for their message."
	MsgPending = "Still waiting for the user. Call ask_wait with this id to keep waiting."
	MsgHidden  = "The viewer page is open but hidden (for example a closed browser pane), so the user may not see these questions. " +
		"Tell the user in one line in the chat that questions are waiting in the viewer, with url. " +
		"If you have the built-in browser tools, show the pane again (preview_start with url). Then call ask_wait with this id."
	MsgUnseen = "Nobody has used the viewer page since these questions were shown, so the user may not have noticed them. " +
		"Tell the user in one line in the chat that questions are waiting in the viewer, with url, then call ask_wait with this id."
	MsgNoViewer = "No viewer page is open, so the user cannot see these questions. " +
		"If you have the built-in browser tools, open url in the built-in browser pane now (preview_start with url) and tell the user in one line; " +
		"otherwise the system browser opens shortly. Then call ask_wait with this id."
	MsgAskInChat = "The user is away from the viewer, so ask these same questions in the chat now, with the built-in question tool " +
		"(AskUserQuestion), which is allowed while they are away: the same questions and options, and for each the passage or code it is about in a short line. " +
		"Keep calling ask for later questions: they come back to the viewer by themselves once the user uses it again."
	MsgHereOn    = "Questions now go to the chat. Ask them with the built-in question tool (AskUserQuestion); re-ask in the chat any question that was waiting in the viewer. Keep calling ask for later questions: it says when to ask in the chat, and the questions come back to the viewer once the user uses it again."
	MsgHereOff   = "Questions go to the viewer again: ask them with the ask tool."
	MsgDismissed = "The user skipped these questions. Continue without these decisions; ask in chat only if one is essential."
	MsgDeferred  = "The user chose to decide some questions later (see deferred). Add each to the spec's open questions with its refs, " +
		"continue without the decision, and ask them again before finishing."
)

// DefaultReopenDelay is the ReopenDelay used when it is zero.
const DefaultReopenDelay = 10 * time.Second

// DefaultNudgeDelay is the NudgeDelay used when it is zero.
const DefaultNudgeDelay = 45 * time.Second

// OpenPane asks Claude to open the viewer in the built-in browser pane of the
// Claude desktop app. It is the default; AI_CONTEXTVIEW_OPEN=browser or never
// turns it off.
const OpenPane = "pane"

// AskModeFromEnv reports whether AI_CONTEXTVIEW_ASK_MODE=always turns ask mode
// on for every session.
func AskModeFromEnv() bool {
	return os.Getenv("AI_CONTEXTVIEW_ASK_MODE") == "always"
}

// AwayFromEnv reports whether AI_CONTEXTVIEW_AWAY=wait keeps unseen
// questions in the viewer; by default (chat) they move to the chat.
func AwayFromEnv() bool {
	return os.Getenv("AI_CONTEXTVIEW_AWAY") == "wait"
}

// OpenInFromEnv returns OpenPane unless AI_CONTEXTVIEW_OPEN is set to another
// value than pane (browser or never).
func OpenInFromEnv() string {
	switch os.Getenv("AI_CONTEXTVIEW_OPEN") {
	case "", OpenPane:
		return OpenPane
	}
	return ""
}

// OpenBrowser opens a URL with the platform opener unless AI_CONTEXTVIEW_OPEN=never.
// It also opens with the pane default: the bridge calls it only when no page
// is open, so it is the fallback when there is no pane.
func OpenBrowser(url string) {
	if os.Getenv("AI_CONTEXTVIEW_OPEN") == "never" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// Not "cmd /c start": cmd would split the URL at "&".
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// syncCwd asks the daemon for the session's folder before a relative path in
// paths is resolved. The hook reports the folder just before this tool runs,
// possibly before this bridge connected; asking makes sure it is used.
func (b *Bridge) syncCwd(ctx context.Context, paths ...string) {
	relative := false
	for _, p := range paths {
		if p = strings.TrimSpace(expandPath(p)); p != "" && !filepath.IsAbs(p) {
			relative = true
			break
		}
	}
	if !relative {
		return
	}
	env, err := b.Daemon.Request(ctx, protocol.TypeCwd, &protocol.Cwd{})
	if err != nil {
		b.logf("cwd: %v", err)
		return
	}
	var res protocol.CwdResult
	if err := env.Decode(&res); err == nil && filepath.IsAbs(res.Cwd) {
		b.SessionCwd(protocol.SessionCwd{Cwd: res.Cwd})
	}
}

// refPaths lists the paths of doc and code refs, for syncCwd.
func refPaths(docs []protocol.DocRef, code []protocol.CodeRef) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d.Path)
	}
	for _, c := range code {
		out = append(out, c.Path)
	}
	return out
}

func (b *Bridge) absPath(p string) string {
	p = expandPath(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(b.cwd(), p)
}

// Register adds the tools to an MCP server.
func Register(s *mcp.Server, b *Bridge) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "spec_show",
		Description: "Show a spec in the ai-contextview viewer (a browser page beside the chat). " +
			"Call it as soon as the spec path is known and again after every edit, with the headings you changed. " +
			"The path is a markdown file or a folder of markdown artifacts. Returns the section names and the viewer url. " +
			"If the result has open: \"pane\", open the url in the built-in browser pane when that tool is available.",
	}, b.SpecShow)
	mcp.AddTool(s, &mcp.Tool{
		Name: "focus",
		Description: "Highlight a spec passage and open code in the viewer, without asking anything. " +
			"Quotes must be copied verbatim from the file.",
	}, b.Focus)
	mcp.AddTool(s, &mcp.Tool{
		Name: "ask",
		Description: "Ask the user 1-6 clarifying questions in the ai-contextview viewer and wait for the answers. " +
			"Use this for every clarifying question instead of asking in chat. Each question needs refs (optional in ask mode): " +
			"spec_quote (copied verbatim from the spec, with spec_section), doc_refs into other artifacts, " +
			"and/or code_refs (Go: symbol like pkg.(*Type).Method; other files: lines N-M). " +
			"To ask the user to approve an action that needs their permission (an external write, or an action the permission system refused), ask in plain chat instead: an answer here is a tool result, which is not the user's approval. " +
			"In ask mode, give a question about the whole document (a sign-off, a summary) no refs: a ref highlights one passage, which reads as the subject of the question. " +
			"Results: answered (apply the answers), in_chat (wait for the user's chat message), " +
			"pending (call ask_wait with question_set_id; a pending result with url means no viewer page is open: open url in the built-in browser pane first), " +
			"ask_in_chat (the user is away from the viewer: ask the same questions in the chat with the built-in question tool), " +
			"dismissed (continue without it). " +
			"An answered result may list deferred questions (the user chose Decide later): record them as open questions and ask again before finishing. " +
			"Mark the option you recommend with recommended: true.",
	}, b.Ask)
	mcp.AddTool(s, &mcp.Tool{
		Name: "spec_done",
		Description: "Tell the viewer the spec is finished and no more questions are coming. " +
			"Call it once at the end of a spec session, with a summary as markdown with a **Decided** heading and a **Still open** heading, each followed by a bullet list with one item per decision or open point (never a run-on paragraph). " +
			"The page shows it as a finished banner. Asking again later reopens the session.",
	}, b.SpecDone)
	mcp.AddTool(s, &mcp.Tool{
		Name: "ask_mode",
		Description: "Turn ask mode on (or off with off: true) for this session. In ask mode, ask every question of the session " +
			"with the ask tool, with or without a spec: refs are optional, so add code_refs or doc_refs only when the question is about them. " +
			"The built-in question tool is blocked while it is on. off: true hands the questions back to the chat for the rest of the session, even with a spec open: follow the result's next. The first markdown document you write becomes the working document " +
			"the user sees beside the questions; spec_show picks another. spec: true is spec mode, which expects a spec: with paths " +
			"(globs) the first written file that matches is shown, without them the file that looks most like a spec. " +
			"If the result has open: \"pane\", open the url in the built-in browser pane when that tool is available.",
	}, b.AskModeTool)
	mcp.AddTool(s, &mcp.Tool{
		Name: "ask_here",
		Description: "Move this session's questions from the ai-contextview viewer to the chat, for a user who is away from the machine " +
			"(for example on Remote Control), or back to the viewer with off: true. In the chat, ask with the built-in question tool; " +
			"the questions come back to the viewer by themselves once the user uses the viewer page again.",
	}, b.AskHereTool)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ask_wait",
		Description: "Keep waiting for a pending question set returned by ask. Same result shape as ask.",
	}, b.AskWaitTool)
}

func (b *Bridge) currentRoot() *specsrc.Root {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.root
}

// SpecShow implements the spec_show tool.
func (b *Bridge) SpecShow(ctx context.Context, _ *mcp.CallToolRequest, in protocol.SpecShowInput) (*mcp.CallToolResult, protocol.SpecShowOutput, error) {
	var out protocol.SpecShowOutput
	if strings.TrimSpace(in.Path) == "" {
		return nil, out, errors.New("path is required")
	}
	b.syncCwd(ctx, in.Path)
	abs := b.absPath(in.Path)
	root, err := specsrc.LoadRoot(abs, expandPath(in.Primary))
	if err != nil {
		return nil, out, err
	}
	if !root.IsDir && len(in.Changed) > 0 {
		return nil, out, errors.New("changed is only allowed when path is a folder; use changed_sections")
	}
	b.mu.Lock()
	askMode := b.askMode
	b.mu.Unlock()
	if in.Provisional && !askMode {
		return nil, out, errors.New("provisional is only allowed in ask mode or spec mode; without it, call spec_show without provisional")
	}
	b.mu.Lock()
	b.root = root
	// A provisional document clears with ask mode, like a picked one.
	b.rootAuto = in.Provisional
	b.mu.Unlock()

	req := &protocol.SpecShow{
		RootAbs: abs, IsDir: root.IsDir, ChangedSections: in.ChangedSections, Changed: in.Changed,
		Provisional: in.Provisional,
	}
	if root.IsDir {
		req.Primary = root.Primary
	}
	reply, err := b.Daemon.Request(ctx, protocol.TypeSpecShow, req)
	if err != nil {
		return nil, out, err
	}
	var res protocol.SpecShowResult
	if err := reply.Decode(&res); err != nil {
		return nil, out, err
	}
	out = protocol.SpecShowOutput{OK: true, URL: b.Daemon.URL(), Open: b.OpenIn, Primary: res.Primary, Sections: res.Sections, Artifacts: res.Artifacts}
	if out.Sections == nil {
		out.Sections = []string{}
	}
	return nil, out, nil
}

// resolveDocs folds spec_quote into the doc refs, resolves their paths and
// pre-checks the quotes. Problems are appended to errs.
func (b *Bridge) resolveDocs(label, specQuote, specSection string, refs []protocol.DocRef, errs *[]string) []protocol.ResolvedDocRef {
	all := refs
	if specQuote != "" {
		all = append([]protocol.DocRef{{Quote: specQuote, Section: specSection}}, refs...)
	}
	if len(all) == 0 {
		return []protocol.ResolvedDocRef{}
	}
	root := b.currentRoot()
	out := make([]protocol.ResolvedDocRef, 0, len(all))
	for _, d := range all {
		abs, ok := b.docPath(root, d.Path)
		if !ok {
			*errs = append(*errs, label+": a doc ref without path means the primary spec file, and no spec is shown; give path, or call spec_show first")
			continue
		}
		name := b.docName(root, abs)
		md := specsrc.IsMarkdown(abs)
		if !md && d.Quote != "" {
			*errs = append(*errs, fmt.Sprintf("%s: doc ref %s is not a markdown file; give lines instead of a quote", label, d.Path))
			continue
		}
		var src []byte
		var err error
		if md {
			src, err = specsrc.ReadPrepared(abs)
		} else {
			src, err = os.ReadFile(abs)
		}
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("%s: doc ref %s does not exist", label, d.Path))
			continue
		}
		ref := protocol.ResolvedDocRef{Path: abs, Quote: d.Quote, Section: d.Section}
		if d.Quote != "" {
			if m := specsrc.Match(src, d.Quote, ""); !m.Found() {
				closest := specsrc.ClosestHeadings(src, d.Quote, 3)
				*errs = append(*errs, fmt.Sprintf("%s: quote %q not found in %s (copy it verbatim from the file); closest sections: %s",
					label, d.Quote, name, strings.Join(closest, ", ")))
				continue
			}
		} else {
			start, end, err := resolve.ParseLines(d.Lines)
			if err != nil {
				*errs = append(*errs, fmt.Sprintf("%s: doc ref %s: %v", label, d.Path, err))
				continue
			}
			n := bytes.Count(src, []byte("\n"))
			if len(src) > 0 && src[len(src)-1] != '\n' {
				n++
			}
			if start > n {
				*errs = append(*errs, fmt.Sprintf("%s: doc ref %s has %d lines; lines %s are past the end", label, d.Path, n, d.Lines))
				continue
			}
			ref.StartLine, ref.EndLine = start, min(end, n)
		}
		ref.Kind = specsrc.InferKind(name, "")
		if root != nil {
			if a := root.Artifact(name); a != nil {
				ref.Kind = a.Kind
			}
		}
		out = append(out, ref)
	}
	return out
}

// docPath turns a doc ref path into an absolute path. An empty path is the
// primary spec file. A relative path is looked up in the spec folder first,
// then in the project directory.
func (b *Bridge) docPath(root *specsrc.Root, p string) (string, bool) {
	if p == "" {
		if root == nil {
			return "", false
		}
		return root.PrimaryAbs(), true
	}
	p = expandPath(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p), true
	}
	if root != nil && root.IsDir {
		if abs := filepath.Join(root.Dir, p); fileExists(abs) {
			return abs, true
		}
	}
	return filepath.Join(b.cwd(), p), true
}

// docName is a doc path for messages: relative to the spec folder, else to
// the project directory.
func (b *Bridge) docName(root *specsrc.Root, abs string) string {
	if root != nil {
		if rel := root.Rel(abs); !strings.HasPrefix(rel, "../") {
			return rel
		}
	}
	if rel, err := filepath.Rel(b.cwd(), abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return abs
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (b *Bridge) resolveCode(ctx context.Context, label string, refs []protocol.CodeRef, errs *[]string) []protocol.ResolvedRef {
	out := make([]protocol.ResolvedRef, 0, len(refs))
	for _, r := range refs {
		res, err := b.Resolver.Resolve(ctx, b.cwd(), r)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("%s: code ref %s: %v", label, r.Path, err))
			continue
		}
		out = append(out, res)
	}
	return out
}

func errList(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.New("fix these and call again:\n- " + strings.Join(errs, "\n- "))
}

// Focus implements the focus tool.
func (b *Bridge) Focus(ctx context.Context, _ *mcp.CallToolRequest, in protocol.FocusInput) (*mcp.CallToolResult, protocol.FocusOutput, error) {
	var out protocol.FocusOutput
	if err := validate.Focus(&in); err != nil {
		return nil, out, err
	}
	b.syncCwd(ctx, refPaths(in.DocRefs, in.CodeRefs)...)
	var errs []string
	docs := b.resolveDocs("focus", in.SpecQuote, in.SpecSection, in.DocRefs, &errs)
	code := b.resolveCode(ctx, "focus", in.CodeRefs, &errs)
	if err := errList(errs); err != nil {
		return nil, out, err
	}
	reply, err := b.Daemon.Request(ctx, protocol.TypeFocus, &protocol.Focus{Docs: docs, Code: code})
	if err != nil {
		return nil, out, err
	}
	var res protocol.FocusResult
	if err := reply.Decode(&res); err != nil {
		return nil, out, err
	}
	out = protocol.FocusOutput{OK: true, Docs: res.Docs, Code: code}
	if out.Docs == nil {
		out.Docs = []protocol.DocMatch{}
	}
	if in.SpecQuote != "" && len(res.Docs) > 0 {
		out.SpecMatch = res.Docs[0].Match
	}
	return nil, out, nil
}

// Ask implements the ask tool.
func (b *Bridge) Ask(ctx context.Context, _ *mcp.CallToolRequest, in protocol.AskInput) (*mcp.CallToolResult, protocol.AskOutput, error) {
	var out protocol.AskOutput
	b.mu.Lock()
	refsOptional := b.askMode
	b.mu.Unlock()
	if err := validate.Ask(&in, refsOptional); err != nil {
		return nil, out, err
	}
	normalizeRecommended(&in)
	ask := &protocol.Ask{QSID: identity.ShortID("qs_"), Title: in.Title}
	var paths []string
	for _, q := range in.Questions {
		paths = append(paths, refPaths(q.DocRefs, q.CodeRefs)...)
	}
	b.syncCwd(ctx, paths...)
	var errs []string
	for _, q := range in.Questions {
		label := "question " + q.ID
		ask.Questions = append(ask.Questions, protocol.AskQuestion{
			ID: q.ID, Text: q.Text, Options: q.Options, MultiSelect: q.MultiSelect,
			Docs: b.resolveDocs(label, q.SpecQuote, q.SpecSection, q.DocRefs, &errs),
			Code: b.resolveCode(ctx, label, q.CodeRefs, &errs),
		})
	}
	if err := errList(errs); err != nil {
		return nil, out, err
	}

	reply, err := b.Daemon.Request(ctx, protocol.TypeAsk, ask)
	if err != nil {
		return nil, out, err
	}
	var shown protocol.AskShown
	if err := reply.Decode(&shown); err != nil {
		return nil, out, err
	}
	if shown.InChat {
		return nil, protocol.AskOutput{Status: protocol.StatusAskInChat, QuestionSetID: ask.QSID, Message: MsgAskInChat}, nil
	}
	for _, m := range shown.Matches {
		if m.Match != protocol.MatchNone {
			continue
		}
		b.cancel(ask.QSID, "quote_not_found")
		for _, q := range ask.Questions {
			if q.ID == m.ID && m.Doc < len(q.Docs) {
				d := q.Docs[m.Doc]
				src, _ := specsrc.ReadPrepared(d.Path)
				return nil, out, fmt.Errorf("question %s: quote %q not found in %s; closest sections: %s",
					q.ID, d.Quote, d.Path, strings.Join(specsrc.ClosestHeadings(src, d.Quote, 3), ", "))
			}
		}
		return nil, out, fmt.Errorf("question %s: quote not found", m.ID)
	}
	if u := b.Daemon.URL(); shown.Viewers == 0 && u != "" {
		if b.OpenIn == OpenPane {
			// No page shows the questions, for example because the user closed
			// the pane. Claude can reopen the pane only once ask returns, so
			// return now and open the system browser only if no page connects.
			b.mu.Lock()
			if b.pending == nil {
				b.pending = map[string]bool{}
			}
			b.pending[ask.QSID] = true
			b.mu.Unlock()
			if b.Open != nil {
				go b.openLater(ask.QSID, u)
			}
			return nil, protocol.AskOutput{Status: protocol.StatusPending, QuestionSetID: ask.QSID, URL: u, Message: MsgNoViewer}, nil
		}
		if b.Open != nil {
			b.Open(u)
		}
	}
	if u := b.Daemon.URL(); shown.Viewers > 0 && shown.Hidden >= shown.Viewers {
		// Every page that shows the questions is hidden: say so in the chat
		// now instead of waiting in silence.
		b.markPending(ask.QSID)
		return nil, protocol.AskOutput{Status: protocol.StatusPending, QuestionSetID: ask.QSID, URL: u, Message: MsgHidden}, nil
	}
	return b.waitNudge(ctx, ask.QSID)
}

func (b *Bridge) markPending(qs string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		b.pending = map[string]bool{}
	}
	b.pending[qs] = true
}

// waitNudge waits for the answers like wait, but returns pending after
// NudgeDelay when nobody has used the viewer page since the questions were
// shown, so that Claude says in the chat that they are waiting.
func (b *Bridge) waitNudge(ctx context.Context, qs string) (*mcp.CallToolResult, protocol.AskOutput, error) {
	nd := b.NudgeDelay
	if nd <= 0 {
		nd = DefaultNudgeDelay
	}
	if b.AskWait <= nd {
		return b.wait(ctx, qs, b.AskWait)
	}
	res, err := b.Daemon.WaitResult(ctx, qs, nd)
	if err != nil || res != nil {
		return b.finish(ctx, qs, res, err)
	}
	if b.seen(ctx) {
		return b.wait(ctx, qs, b.AskWait-nd)
	}
	if !b.StayInViewer {
		// Nobody is at the viewer, perhaps because the user follows the
		// session from another device: move the questions to the chat.
		if res, err := b.askHere(ctx, true, true); err == nil && res.InChat {
			b.mu.Lock()
			delete(b.pending, qs)
			b.mu.Unlock()
			return nil, protocol.AskOutput{Status: protocol.StatusAskInChat, QuestionSetID: qs, Message: MsgAskInChat}, nil
		} else if err == nil {
			// Someone used the page meanwhile.
			return b.wait(ctx, qs, b.AskWait-nd)
		}
	}
	b.markPending(qs)
	return nil, protocol.AskOutput{Status: protocol.StatusPending, QuestionSetID: qs, URL: b.Daemon.URL(), Message: MsgUnseen}, nil
}

// askHere sends ask_here to the daemon.
func (b *Bridge) askHere(ctx context.Context, on, auto bool) (*protocol.AskHereResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reply, err := b.Daemon.Request(ctx, protocol.TypeAskHere, &protocol.AskHere{On: on, Auto: auto})
	if err != nil {
		return nil, err
	}
	var res protocol.AskHereResult
	if err := reply.Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// AskHereTool implements the ask_here tool.
func (b *Bridge) AskHereTool(ctx context.Context, _ *mcp.CallToolRequest, in protocol.AskHereInput) (*mcp.CallToolResult, protocol.AskHereOutput, error) {
	res, err := b.askHere(ctx, !in.Off, false)
	if err != nil {
		return nil, protocol.AskHereOutput{}, err
	}
	if res.Closed != "" {
		b.mu.Lock()
		delete(b.pending, res.Closed)
		b.mu.Unlock()
	}
	out := protocol.AskHereOutput{InChat: res.InChat, Closed: res.Closed, Message: MsgHereOff}
	if res.InChat {
		out.Message = MsgHereOn
	}
	return nil, out, nil
}

// seen reports whether someone used the viewer page since the pending
// question set was shown. Without an answer from the daemon, or from a
// daemon that does not track it, it reports true: no nudge.
func (b *Bridge) seen(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reply, err := b.Daemon.Request(ctx, protocol.TypeViewers, &protocol.Viewers{})
	if err != nil {
		return true
	}
	var res protocol.ViewersResult
	if reply.Decode(&res) != nil || !res.Presence {
		return true
	}
	return res.Seen
}

// openLater opens the system browser for a question set after ReopenDelay,
// unless a page shows the workspace by then or the set has a result.
func (b *Bridge) openLater(qs, url string) {
	d := b.ReopenDelay
	if d <= 0 {
		d = DefaultReopenDelay
	}
	time.Sleep(d)
	if b.Daemon.HasResult(qs) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reply, err := b.Daemon.Request(ctx, protocol.TypeViewers, &protocol.Viewers{})
	if err == nil {
		var res protocol.ViewersResult
		if err = reply.Decode(&res); err == nil && res.Viewers > 0 {
			return
		}
	}
	if err != nil {
		b.logf("viewers %s: %v", qs, err)
	}
	b.logf("opening the default browser for %s: no viewer page after %s", qs, d)
	b.Open(url)
}

// SpecDone implements the spec_done tool.
func (b *Bridge) SpecDone(ctx context.Context, _ *mcp.CallToolRequest, in protocol.SpecDoneInput) (*mcp.CallToolResult, protocol.SpecDoneOutput, error) {
	req := &protocol.Done{Summary: strings.TrimSpace(in.Summary)}
	if err := req.Validate(); err != nil {
		return nil, protocol.SpecDoneOutput{}, err
	}
	if _, err := b.Daemon.Request(ctx, protocol.TypeDone, req); err != nil {
		return nil, protocol.SpecDoneOutput{}, err
	}
	return nil, protocol.SpecDoneOutput{OK: true, URL: b.Daemon.URL()}, nil
}

// recommendedSuffix is the built-in question tool's convention for marking
// the recommended option; it is turned into the recommended flag.
const recommendedSuffix = "(recommended)"

func normalizeRecommended(in *protocol.AskInput) {
	for qi := range in.Questions {
		opts := in.Questions[qi].Options
		for oi := range opts {
			l := strings.TrimSpace(opts[oi].Label)
			if strings.HasSuffix(strings.ToLower(l), recommendedSuffix) {
				opts[oi].Label = strings.TrimSpace(l[:len(l)-len(recommendedSuffix)])
				opts[oi].Recommended = true
			}
		}
	}
}

// AskModeTool implements the ask_mode tool.
func (b *Bridge) AskModeTool(ctx context.Context, _ *mcp.CallToolRequest, in protocol.AskModeInput) (*mcp.CallToolResult, protocol.AskModeOutput, error) {
	on := !in.Off
	req := &protocol.AskMode{On: on, Spec: on && in.Spec}
	if req.Spec {
		b.syncCwd(ctx, in.Paths...)
		for _, p := range in.Paths {
			if p = strings.TrimSpace(p); p != "" {
				req.Paths = append(req.Paths, b.specPath(p))
			}
		}
	}
	reply, err := b.Daemon.Request(ctx, protocol.TypeAskMode, req)
	if err != nil {
		return nil, protocol.AskModeOutput{}, err
	}
	var res protocol.AskModeResult
	if err := reply.Decode(&res); err != nil {
		return nil, protocol.AskModeOutput{}, err
	}
	if res.Shown != "" {
		// The daemon also sends doc_shown; follow it here too, so a
		// spec_quote right after this call quotes the shown file.
		b.DocShown(protocol.DocShown{RootAbs: res.Shown})
	}
	b.mu.Lock()
	b.askMode = on
	if !on && b.rootAuto {
		// The daemon clears a picked document when ask mode ends.
		b.root, b.rootAuto = nil, false
		if c, ok := b.Daemon.(autoClearer); ok {
			c.ClearAutoSpec()
		}
	}
	b.mu.Unlock()
	out := protocol.AskModeOutput{On: on, Spec: req.Spec, Shown: res.Shown}
	if on {
		out.URL = b.Daemon.URL()
		out.Open = b.OpenIn
	} else {
		out.Next = protocol.OffNext
	}
	return nil, out, nil
}

// specPath makes a spec-mode path absolute. A relative path with no glob
// that names no file under the session's folder is looked up in the other
// git worktrees of the session's repository, and of the folder the bridge
// started in: a spec often lives on another worktree's branch. When several
// have it, the most recently changed copy wins. With no copy anywhere, the
// path stays relative to the session's folder and waits for a write.
func (b *Bridge) specPath(p string) string {
	abs := b.absPath(p)
	rel := expandPath(p)
	if filepath.IsAbs(rel) || strings.ContainsAny(rel, "*?[") || isFile(abs) {
		return abs
	}
	best, bestTime := "", time.Time{}
	seen := map[string]bool{}
	for _, dir := range []string{b.cwd(), b.Cwd} {
		root, _ := gitwt.Root(dir)
		if root == "" {
			continue
		}
		sub, err := filepath.Rel(root, dir)
		if err != nil {
			continue
		}
		for _, wt := range gitwt.Worktrees(dir) {
			c := filepath.Join(wt, sub, rel)
			if seen[c] {
				continue
			}
			seen[c] = true
			if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() && fi.ModTime().After(bestTime) {
				best, bestTime = c, fi.ModTime()
			}
		}
	}
	if best == "" {
		return abs
	}
	return best
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// DocShown follows the daemon when it made a written file the working
// document, so spec_quote quotes that file.
func (b *Bridge) DocShown(d protocol.DocShown) {
	root, err := specsrc.LoadRoot(d.RootAbs, "")
	if err != nil {
		b.logf("doc_shown %s: %v", d.RootAbs, err)
		return
	}
	b.mu.Lock()
	b.root = root
	b.rootAuto = true
	b.mu.Unlock()
}

// autoClearer is implemented by appconn.Client: it forgets a picked
// document, so a reconnect does not show it again.
type autoClearer interface {
	ClearAutoSpec()
}

// AskWaitTool implements the ask_wait tool.
func (b *Bridge) AskWaitTool(ctx context.Context, _ *mcp.CallToolRequest, in protocol.AskWaitInput) (*mcp.CallToolResult, protocol.AskOutput, error) {
	b.mu.Lock()
	known := b.pending[in.QuestionSetID]
	b.mu.Unlock()
	if !known && !b.Daemon.HasResult(in.QuestionSetID) {
		return nil, protocol.AskOutput{}, fmt.Errorf("unknown question_set_id %q", in.QuestionSetID)
	}
	return b.wait(ctx, in.QuestionSetID, b.AskWait)
}

func (b *Bridge) wait(ctx context.Context, qs string, d time.Duration) (*mcp.CallToolResult, protocol.AskOutput, error) {
	res, err := b.Daemon.WaitResult(ctx, qs, d)
	return b.finish(ctx, qs, res, err)
}

// finish turns what WaitResult returned into the ask output.
func (b *Bridge) finish(ctx context.Context, qs string, res *protocol.AskResult, err error) (*mcp.CallToolResult, protocol.AskOutput, error) {
	if err != nil {
		if ctx.Err() != nil {
			b.cancel(qs, "client_cancelled")
		}
		return nil, protocol.AskOutput{}, err
	}
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[string]bool{}
	}
	if res == nil {
		b.pending[qs] = true
	} else {
		delete(b.pending, qs)
	}
	b.mu.Unlock()
	if res == nil {
		return nil, protocol.AskOutput{Status: protocol.StatusPending, QuestionSetID: qs, Message: MsgPending}, nil
	}
	out := protocol.AskOutput{Status: res.Status, Answers: res.Answers, Deferred: res.Deferred, KeepOpen: res.KeepOpen}
	switch res.Status {
	case protocol.StatusAnswered:
		if len(res.Deferred) > 0 {
			out.Message = MsgDeferred
		}
	case protocol.StatusInChat:
		out.Message = MsgInChat
	case protocol.StatusAskInChat:
		out.QuestionSetID = qs
		out.Message = MsgAskInChat
	case protocol.StatusDismissed:
		out.Message = MsgDismissed
	}
	return nil, out, nil
}

func (b *Bridge) cancel(qs, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := b.Daemon.Request(ctx, protocol.TypeAskCancel, &protocol.AskCancel{QSID: qs, Reason: reason}); err != nil {
		b.logf("ask_cancel %s: %v", qs, err)
	}
}
