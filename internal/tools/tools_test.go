package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rrgmc/ai-contextview/internal/appconn"
	"github.com/rrgmc/ai-contextview/internal/fakeapp"
	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/resolve"
	"github.com/rrgmc/ai-contextview/internal/testenv"
)

const specMD = `# Retry spec

## Retry policy

All retries use **exponential backoff** capped at 30s.

## Errors

Errors are wrapped.
`

type env struct {
	cwd    string
	fake   *fakeapp.Server
	client *appconn.Client
	bridge *Bridge

	mu     sync.Mutex
	opened []string
}

func (e *env) openedURLs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.opened...)
}

// shortSocket returns a socket path short enough for macOS (104 bytes).
func shortSocket(t *testing.T) string {
	dir := testenv.ShortTempDir(t, "sv")
	return filepath.Join(dir, "s.sock")
}

func setup(t *testing.T, onAsk func(string, *protocol.Ask) *protocol.AskResult) *env {
	cwd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "docs/spec.md"), []byte(specMD), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "main.txt"), []byte("a\nb\nc\nd\n"), 0o644))

	sock := shortSocket(t)
	fake := &fakeapp.Server{Socket: sock, OnAsk: onAsk}
	require.NoError(t, fake.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = fake.Serve(ctx) }()

	client := appconn.New(appconn.Options{
		Socket: sock,
		Hello:  protocol.Hello{WS: "ws_test", Cwd: cwd, ProjectName: "p"},
		Logf:   t.Logf,
	})
	t.Cleanup(client.Close)
	e := &env{cwd: cwd, fake: fake, client: client}
	e.bridge = &Bridge{
		Cwd: cwd, Daemon: client, Resolver: resolve.New(), AskWait: 2 * time.Second,
		Open: func(u string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.opened = append(e.opened, u)
		},
	}
	return e
}

func (e *env) show(t *testing.T) {
	_, out, err := e.bridge.SpecShow(context.Background(), nil, protocol.SpecShowInput{Path: "docs/spec.md"})
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.Equal(t, []string{"Retry spec", "Retry policy", "Errors"}, out.Sections)
	assert.Contains(t, out.URL, "/w/ws_test")
}

func TestSpecShowOpenIn(t *testing.T) {
	e := setup(t, nil)
	_, out, err := e.bridge.SpecShow(context.Background(), nil, protocol.SpecShowInput{Path: "docs/spec.md"})
	require.NoError(t, err)
	assert.Empty(t, out.Open)

	e.bridge.OpenIn = OpenPane
	_, out, err = e.bridge.SpecShow(context.Background(), nil, protocol.SpecShowInput{Path: "docs/spec.md"})
	require.NoError(t, err)
	assert.Equal(t, OpenPane, out.Open)
}

func TestSpecShowProvisional(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	in := protocol.SpecShowInput{Path: "docs/spec.md", Provisional: true}
	_, _, err := e.bridge.SpecShow(ctx, nil, in)
	require.Error(t, err, "provisional needs ask mode")

	_, _, err = e.bridge.AskModeTool(ctx, nil, protocol.AskModeInput{Spec: true})
	require.NoError(t, err)
	_, out, err := e.bridge.SpecShow(ctx, nil, in)
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.NotNil(t, e.bridge.currentRoot())

	// Ask mode off clears it, like a document the daemon picked.
	_, _, err = e.bridge.AskModeTool(ctx, nil, protocol.AskModeInput{Off: true})
	require.NoError(t, err)
	assert.Nil(t, e.bridge.currentRoot())
}

func TestOpenInFromEnv(t *testing.T) {
	for value, want := range map[string]string{"": OpenPane, "pane": OpenPane, "browser": "", "never": ""} {
		t.Setenv("AI_CONTEXTVIEW_OPEN", value)
		assert.Equal(t, want, OpenInFromEnv(), "AI_CONTEXTVIEW_OPEN=%q", value)
	}
}

func question(quote string) protocol.AskInput {
	return protocol.AskInput{Title: "T", Questions: []protocol.QuestionInput{{
		ID: "q1", Text: "Reset?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
		SpecQuote: quote, SpecSection: "Retry policy",
		CodeRefs: []protocol.CodeRef{{Path: "main.txt", Lines: "2-3"}},
	}}}
}

func TestAskAnswered(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusAnswered,
			Answers: []protocol.Answer{{ID: "q1", Selected: []string{"No"}}}}
	})
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAnswered, out.Status)
	require.Len(t, out.Answers, 1)
	assert.Equal(t, []string{"No"}, out.Answers[0].Selected)
	assert.Len(t, e.openedURLs(), 1, "viewers 0 opens the page")

	// The daemon received resolved refs.
	var ask protocol.Ask
	for _, m := range e.fake.Received() {
		if m.Type == protocol.TypeAsk {
			require.NoError(t, m.Decode(&ask))
		}
	}
	require.Len(t, ask.Questions, 1)
	assert.Equal(t, filepath.Join(e.cwd, "docs/spec.md"), ask.Questions[0].Docs[0].Path)
	assert.Equal(t, 2, ask.Questions[0].Code[0].StartLine)
}

func TestAskQuoteNotFound(t *testing.T) {
	e := setup(t, nil)
	e.show(t)
	_, _, err := e.bridge.Ask(context.Background(), nil, question("we honour whatever the server says about delays"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in spec.md")
	assert.Contains(t, err.Error(), "closest sections")
}

func TestAskWithoutSpecShow(t *testing.T) {
	e := setup(t, nil)
	_, _, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff"))
	assert.ErrorContains(t, err, "call spec_show first")
}

func TestAskPendingThenWait(t *testing.T) {
	e := setup(t, nil)
	e.bridge.AskWait = 200 * time.Millisecond
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("capped at 30s"))
	require.NoError(t, err)
	require.Equal(t, protocol.StatusPending, out.Status)
	qs := out.QuestionSetID
	require.NotEmpty(t, qs)

	// The answer arrives while nobody waits; ask_wait returns it buffered.
	require.NoError(t, e.fake.Answer("ws_test", &protocol.AskResult{QSID: qs, Status: protocol.StatusInChat, KeepOpen: []string{"q1"}}))
	require.Eventually(t, func() bool { return e.client.HasResult(qs) }, 2*time.Second, 10*time.Millisecond)
	_, out, err = e.bridge.AskWaitTool(context.Background(), nil, protocol.AskWaitInput{QuestionSetID: qs})
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusInChat, out.Status)
	assert.Equal(t, []string{"q1"}, out.KeepOpen)
	assert.Equal(t, MsgInChat, out.Message)

	_, _, err = e.bridge.AskWaitTool(context.Background(), nil, protocol.AskWaitInput{QuestionSetID: "qs_nope"})
	assert.Error(t, err)
}

func TestAskCancelled(t *testing.T) {
	e := setup(t, nil)
	e.show(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := e.bridge.Ask(ctx, nil, question("capped at 30s"))
	require.Error(t, err)
	require.Eventually(t, func() bool {
		for _, m := range e.fake.Received() {
			if m.Type == protocol.TypeAskCancel {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
}

func TestAskValidation(t *testing.T) {
	e := setup(t, nil)
	e.show(t)
	in := protocol.AskInput{Questions: []protocol.QuestionInput{{ID: "q1", Text: "x", Options: []protocol.Option{{Label: "a"}}}}}
	_, _, err := e.bridge.Ask(context.Background(), nil, in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "options")
	assert.Contains(t, err.Error(), "needs at least one of")
}

func TestReconnectRedeliversPendingAsk(t *testing.T) {
	e := setup(t, nil)
	e.bridge.AskWait = 100 * time.Millisecond
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("capped at 30s"))
	require.NoError(t, err)
	require.Equal(t, protocol.StatusPending, out.Status)

	// Kill the fake daemon and start a new one on the same socket.
	sock := e.fake.Socket
	e.fake.Close()
	fake2 := &fakeapp.Server{Socket: sock}
	require.NoError(t, fake2.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = fake2.Serve(ctx) }()

	require.Eventually(t, func() bool {
		var types []string
		for _, m := range fake2.Received() {
			types = append(types, m.Type)
		}
		return assert.ObjectsAreEqual([]string{protocol.TypeHello, protocol.TypeSpecShow, protocol.TypeAsk}, types)
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, fake2.Answer("ws_test", &protocol.AskResult{QSID: out.QuestionSetID, Status: protocol.StatusDismissed}))
	e.bridge.AskWait = 2 * time.Second
	_, out, err = e.bridge.AskWaitTool(context.Background(), nil, protocol.AskWaitInput{QuestionSetID: out.QuestionSetID})
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusDismissed, out.Status)
}

func TestRecommendedOption(t *testing.T) {
	var got *protocol.Ask
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		got = a
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusDismissed}
	})
	e.show(t)
	in := question("capped at 30s")
	in.Questions[0].Options = []protocol.Option{{Label: "Yes (Recommended)"}, {Label: "No"}}
	_, _, err := e.bridge.Ask(context.Background(), nil, in)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, protocol.Option{Label: "Yes", Recommended: true}, got.Questions[0].Options[0])
	assert.False(t, got.Questions[0].Options[1].Recommended)

	in = question("capped at 30s")
	in.Questions[0].Options = []protocol.Option{{Label: "Yes", Recommended: true}, {Label: "No", Recommended: true}}
	_, _, err = e.bridge.Ask(context.Background(), nil, in)
	assert.ErrorContains(t, err, "at most 1")
}

func TestSpecDone(t *testing.T) {
	e := setup(t, nil)
	e.show(t)
	_, out, err := e.bridge.SpecDone(context.Background(), nil, protocol.SpecDoneInput{Summary: "  done  "})
	require.NoError(t, err)
	assert.True(t, out.OK)
	var got protocol.Done
	for _, m := range e.fake.Received() {
		if m.Type == protocol.TypeDone {
			require.NoError(t, m.Decode(&got))
		}
	}
	assert.Equal(t, "done", got.Summary)
}

func TestDeferredMessage(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusAnswered,
			Answers:  []protocol.Answer{{ID: "q1", Selected: []string{}, Deferred: true}},
			Deferred: []string{"q1"}}
	})
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAnswered, out.Status)
	assert.Equal(t, []string{"q1"}, out.Deferred)
	assert.Equal(t, MsgDeferred, out.Message)
}

func TestAskNoViewerPaneReturnsAtOnce(t *testing.T) {
	e := setup(t, nil)
	e.bridge.OpenIn = OpenPane
	e.bridge.ReopenDelay = 50 * time.Millisecond
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusPending, out.Status)
	assert.Equal(t, MsgNoViewer, out.Message)
	assert.Contains(t, out.URL, "/w/ws_test")
	require.NotEmpty(t, out.QuestionSetID)
	assert.Empty(t, e.openedURLs(), "the system browser waits for Claude to reopen the pane")

	// No page connects: the system browser opens after the delay.
	require.Eventually(t, func() bool { return len(e.openedURLs()) == 1 }, 2*time.Second, 10*time.Millisecond)

	// ask_wait knows the question set.
	require.NoError(t, e.fake.Answer("ws_test", &protocol.AskResult{QSID: out.QuestionSetID, Status: protocol.StatusDismissed}))
	_, res, err := e.bridge.AskWaitTool(context.Background(), nil, protocol.AskWaitInput{QuestionSetID: out.QuestionSetID})
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusDismissed, res.Status)
}

func TestAskNoViewerPaneReopened(t *testing.T) {
	e := setup(t, nil)
	e.bridge.OpenIn = OpenPane
	e.bridge.ReopenDelay = 50 * time.Millisecond
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusPending, out.Status)

	// Claude reopens the pane before the delay ends.
	e.fake.SetViewers(1)
	time.Sleep(300 * time.Millisecond)
	assert.Empty(t, e.openedURLs(), "a page connected, so the system browser stays closed")
}

func TestAskWithViewerPaneWaits(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusAnswered,
			Answers: []protocol.Answer{{ID: "q1", Selected: []string{"Yes"}}}}
	})
	e.fake.SetViewers(1)
	e.bridge.OpenIn = OpenPane
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAnswered, out.Status)
	assert.Empty(t, out.URL)
	assert.Empty(t, e.openedURLs())
}

func TestAskModeMakesRefsOptional(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusAnswered,
			Answers: []protocol.Answer{{ID: "q1", Selected: []string{"Yes"}}}}
	})
	e.fake.SetViewers(1)
	noRefs := protocol.AskInput{Questions: []protocol.QuestionInput{{
		ID: "q1", Text: "Use tabs?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
	}}}

	// Without ask mode a question needs a ref.
	_, _, err := e.bridge.Ask(context.Background(), nil, noRefs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs at least one of")

	_, mode, err := e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{})
	require.NoError(t, err)
	assert.True(t, mode.On)
	assert.Contains(t, mode.URL, "/w/ws_test")

	// In ask mode, with no spec shown, it does not.
	_, out, err := e.bridge.Ask(context.Background(), nil, noRefs)
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAnswered, out.Status)

	var sent protocol.AskMode
	for _, m := range e.fake.Received() {
		if m.Type == protocol.TypeAskMode {
			require.NoError(t, m.Decode(&sent))
		}
	}
	assert.True(t, sent.On)

	_, mode, err = e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{Off: true})
	require.NoError(t, err)
	assert.False(t, mode.On)
	assert.Empty(t, mode.URL)
	assert.Equal(t, protocol.OffNext, mode.Next, "off tells Claude to ask in the chat")
	_, _, err = e.bridge.Ask(context.Background(), nil, noRefs)
	require.Error(t, err)
}

func TestDocRefsWithoutSpecAndByLines(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusDismissed}
	})
	e.fake.SetViewers(1)
	require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "notes.md"), []byte("# Notes\n\nKeep the cache small.\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "app.yaml"), []byte("a: 1\nb: 2\nc: 3\n"), 0o644))
	q := func(refs ...protocol.DocRef) protocol.AskInput {
		return protocol.AskInput{Questions: []protocol.QuestionInput{{
			ID: "q1", Text: "Ok?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}}, DocRefs: refs,
		}}}
	}

	// No spec shown: a doc ref with a path works, by quote or by lines.
	_, _, err := e.bridge.Ask(context.Background(), nil, q(
		protocol.DocRef{Path: "notes.md", Quote: "Keep the cache small."},
		protocol.DocRef{Path: "app.yaml", Lines: "2-9"},
	))
	require.NoError(t, err)
	var ask protocol.Ask
	for _, m := range e.fake.Received() {
		if m.Type == protocol.TypeAsk {
			require.NoError(t, m.Decode(&ask))
		}
	}
	require.Len(t, ask.Questions[0].Docs, 2)
	yaml := ask.Questions[0].Docs[1]
	assert.Equal(t, filepath.Join(e.cwd, "app.yaml"), yaml.Path)
	assert.Equal(t, 2, yaml.StartLine)
	assert.Equal(t, 3, yaml.EndLine, "the range is cut at the end of the file")
	assert.Empty(t, yaml.Quote)

	// A doc ref without path needs a spec.
	_, _, err = e.bridge.Ask(context.Background(), nil, q(protocol.DocRef{Quote: "Keep the cache small."}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no spec is shown")

	// A file that is not markdown takes lines, not a quote.
	_, _, err = e.bridge.Ask(context.Background(), nil, q(protocol.DocRef{Path: "app.yaml", Quote: "b: 2 and more"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "give lines instead of a quote")

	// Exactly one of quote or lines.
	_, _, err = e.bridge.Ask(context.Background(), nil, q(protocol.DocRef{Path: "notes.md", Quote: "Keep the cache", Lines: "1"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of quote or lines")

	// Lines past the end.
	_, _, err = e.bridge.Ask(context.Background(), nil, q(protocol.DocRef{Path: "app.yaml", Lines: "7"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "past the end")
}

func TestAskModeFromEnv(t *testing.T) {
	t.Setenv("AI_CONTEXTVIEW_ASK_MODE", "always")
	assert.True(t, AskModeFromEnv())
	t.Setenv("AI_CONTEXTVIEW_ASK_MODE", "")
	assert.False(t, AskModeFromEnv())
}

func TestDocShownAndSpecModePaths(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusDismissed}
	})
	e.fake.SetViewers(1)

	_, mode, err := e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{Spec: true, Paths: []string{"docs/*.md", " "}})
	require.NoError(t, err)
	assert.True(t, mode.Spec)
	var sent protocol.AskMode
	for _, m := range e.fake.Received() {
		if m.Type == protocol.TypeAskMode {
			require.NoError(t, m.Decode(&sent))
		}
	}
	assert.Equal(t, []string{filepath.Join(e.cwd, "docs/*.md")}, sent.Paths)

	// Without a working document, spec_quote has nothing to quote.
	_, _, err = e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.Error(t, err)

	// The daemon picked docs/spec.md: spec_quote now quotes it.
	e.bridge.DocShown(protocol.DocShown{RootAbs: filepath.Join(e.cwd, "docs/spec.md")})
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusDismissed, out.Status)
}

func TestAskModeOffForgetsPickedDoc(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusDismissed}
	})
	e.fake.SetViewers(1)
	_, _, err := e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{})
	require.NoError(t, err)
	e.bridge.DocShown(protocol.DocShown{RootAbs: filepath.Join(e.cwd, "docs/spec.md")})
	_, _, err = e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)

	// Ask mode off: spec_quote has nothing to quote any more.
	_, _, err = e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{Off: true})
	require.NoError(t, err)
	_, _, err = e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.Error(t, err)

	// A spec shown with spec_show survives ask mode off.
	e.show(t)
	_, _, err = e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{})
	require.NoError(t, err)
	_, _, err = e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{Off: true})
	require.NoError(t, err)
	_, _, err = e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
}

func TestSessionCwdResolvesRelativePaths(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusDismissed}
	})
	e.fake.SetViewers(1)
	wt := filepath.Join(e.cwd, "wt")
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "docs", "only-here.md"), []byte("# Only here\n\nIn the worktree.\n"), 0o644))
	ask := protocol.AskInput{Questions: []protocol.QuestionInput{{
		ID: "q1", Text: "Ok?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
		DocRefs: []protocol.DocRef{{Path: "docs/only-here.md", Quote: "In the worktree."}},
	}}}
	// Against the bridge's own folder, the file is missing.
	_, _, err := e.bridge.Ask(context.Background(), nil, ask)
	require.Error(t, err)

	e.bridge.SessionCwd(protocol.SessionCwd{Cwd: wt})
	_, _, err = e.bridge.Ask(context.Background(), nil, ask)
	require.NoError(t, err)
}

func TestRelativePathsAskTheDaemonForTheSessionFolder(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusDismissed}
	})
	e.fake.SetViewers(1)
	wt := filepath.Join(e.cwd, "wt")
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "docs", "only-here.md"), []byte("# Only here\n\nIn the worktree.\n"), 0o644))
	ask := protocol.AskInput{Questions: []protocol.QuestionInput{{
		ID: "q1", Text: "Ok?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
		DocRefs: []protocol.DocRef{{Path: "docs/only-here.md", Quote: "In the worktree."}},
	}}}
	// The hook reported the folder, but the bridge was not told yet: it
	// asks the daemon before resolving the relative path.
	e.fake.SetSessionCwd(wt)
	_, _, err := e.bridge.Ask(context.Background(), nil, ask)
	require.NoError(t, err)

	// Spec-mode paths resolve against it too.
	_, _, err = e.bridge.AskModeTool(context.Background(), nil, protocol.AskModeInput{Spec: true, Paths: []string{"docs/**"}})
	require.NoError(t, err)
	var mode protocol.AskMode
	for _, env := range e.fake.Received() {
		if env.Type == protocol.TypeAskMode {
			require.NoError(t, env.Decode(&mode))
		}
	}
	assert.Equal(t, []string{filepath.Join(wt, "docs/**")}, mode.Paths)
}

func TestAskHiddenViewerReturnsPending(t *testing.T) {
	e := setup(t, nil)
	e.fake.SetViewers(1)
	e.fake.SetPresence(1, false)
	e.show(t)
	start := time.Now()
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second, "returns at once")
	assert.Equal(t, protocol.StatusPending, out.Status)
	assert.Equal(t, MsgHidden, out.Message)
	assert.NotEmpty(t, out.URL)
	assert.Empty(t, e.openedURLs(), "a hidden page is not replaced by the system browser")

	// ask_wait keeps waiting for the same set.
	require.NoError(t, e.fake.Answer("ws_test", &protocol.AskResult{QSID: out.QuestionSetID, Status: protocol.StatusAnswered,
		Answers: []protocol.Answer{{ID: "q1", Selected: []string{"Yes"}}}}))
	_, out, err = e.bridge.AskWaitTool(context.Background(), nil, protocol.AskWaitInput{QuestionSetID: out.QuestionSetID})
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAnswered, out.Status)
}

func TestAskMovesToChatWhenUnseen(t *testing.T) {
	e := setup(t, nil)
	e.fake.SetViewers(1)
	e.fake.SetPresence(0, false)
	e.bridge.NudgeDelay = 50 * time.Millisecond
	e.show(t)
	start := time.Now()
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), e.bridge.AskWait, "returns at the nudge, not at AskWait")
	assert.Equal(t, protocol.StatusAskInChat, out.Status)
	assert.Equal(t, MsgAskInChat, out.Message)

	// The next question goes to the chat at once.
	start = time.Now()
	_, out, err = e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 40*time.Millisecond)
	assert.Equal(t, protocol.StatusAskInChat, out.Status)
}

func TestAskHereTool(t *testing.T) {
	e := setup(t, nil)
	e.fake.SetViewers(1)
	e.show(t)
	_, here, err := e.bridge.AskHereTool(context.Background(), nil, protocol.AskHereInput{})
	require.NoError(t, err)
	assert.True(t, here.InChat)
	assert.Equal(t, MsgHereOn, here.Message)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAskInChat, out.Status)

	_, here, err = e.bridge.AskHereTool(context.Background(), nil, protocol.AskHereInput{Off: true})
	require.NoError(t, err)
	assert.False(t, here.InChat)
	assert.Equal(t, MsgHereOff, here.Message)
}

func TestAskNudgesWhenUnseen(t *testing.T) {
	e := setup(t, nil)
	e.fake.SetViewers(1)
	e.fake.SetPresence(0, false)
	e.bridge.StayInViewer = true
	e.bridge.NudgeDelay = 50 * time.Millisecond
	e.show(t)
	start := time.Now()
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), e.bridge.AskWait, "returns at the nudge, not at AskWait")
	assert.Equal(t, protocol.StatusPending, out.Status)
	assert.Equal(t, MsgUnseen, out.Message)
	assert.NotEmpty(t, out.URL)
}

func TestAskNoNudgeWhenSeen(t *testing.T) {
	e := setup(t, nil)
	e.fake.SetViewers(1)
	e.fake.SetPresence(0, true)
	e.bridge.NudgeDelay = 50 * time.Millisecond
	e.bridge.AskWait = 300 * time.Millisecond
	e.show(t)
	start := time.Now()
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond, "waits the whole AskWait")
	assert.Equal(t, protocol.StatusPending, out.Status)
	assert.Equal(t, MsgPending, out.Message)
}

func TestAskAnswerBeforeNudge(t *testing.T) {
	e := setup(t, func(_ string, a *protocol.Ask) *protocol.AskResult {
		return &protocol.AskResult{QSID: a.QSID, Status: protocol.StatusAnswered,
			Answers: []protocol.Answer{{ID: "q1", Selected: []string{"Yes"}}}}
	})
	e.fake.SetViewers(1)
	e.fake.SetPresence(0, false)
	e.bridge.NudgeDelay = time.Second
	e.show(t)
	_, out, err := e.bridge.Ask(context.Background(), nil, question("retries use exponential backoff capped at 30s"))
	require.NoError(t, err)
	assert.Equal(t, protocol.StatusAnswered, out.Status)
}

func TestSpecPathFindsOtherWorktrees(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	main := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(main, ".git"), 0o755))
	wt := func(name string) string {
		root := filepath.Join(main, ".claude", "worktrees", name)
		meta := filepath.Join(main, ".git", "worktrees", name)
		require.NoError(t, os.MkdirAll(meta, 0o755))
		require.NoError(t, os.MkdirAll(root, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(meta, "gitdir"), []byte(filepath.Join(root, ".git")+"\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(meta, "commondir"), []byte("../..\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+meta+"\n"), 0o644))
		return root
	}
	spec := filepath.Join("docs", "specs", "SW-1.md")
	write := func(root string, at time.Time) string {
		p := filepath.Join(root, spec)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("# SW-1\n"), 0o644))
		require.NoError(t, os.Chtimes(p, at, at))
		return p
	}
	now := time.Now()
	older := write(wt("older"), now.Add(-time.Hour))
	newer := write(wt("newer"), now)
	session := wt("session")

	b := &Bridge{Cwd: session}
	// The session's folder has no copy: the newest worktree copy wins.
	assert.Equal(t, newer, b.specPath(spec))
	assert.NotEqual(t, older, b.specPath(spec))
	// A glob and a missing file stay relative to the session's folder.
	assert.Equal(t, filepath.Join(session, "docs/specs/*.md"), b.specPath("docs/specs/*.md"))
	assert.Equal(t, filepath.Join(session, "docs/specs/SW-2.md"), b.specPath("docs/specs/SW-2.md"))
	// The session's own copy wins, even when older.
	own := write(session, now.Add(-2*time.Hour))
	assert.Equal(t, own, b.specPath(spec))
	// Outside a repository nothing changes.
	out := &Bridge{Cwd: t.TempDir()}
	assert.Equal(t, filepath.Join(out.Cwd, spec), out.specPath(spec))
}
