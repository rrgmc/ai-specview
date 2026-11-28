package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rrgmc/ai-contextview/internal/daemon"
	"github.com/rrgmc/ai-contextview/internal/identity"
	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/testenv"
)

const askEvent = `{"session_id":"s","cwd":"/x","hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{}}`

// claudePID stands in for the Claude Code process that started the bridge.
const claudePID = 424242

type env struct {
	t    *testing.T
	sock string
	conn net.Conn
	sc   *bufio.Scanner
	n    int
}

func startDaemon(t *testing.T) *env {
	dir := testenv.ShortTempDir(t, "svh")
	sock := filepath.Join(dir, "d.sock")
	d := daemon.New("test")
	t.Cleanup(d.RequestShutdown)
	d.BaseURL = "http://127.0.0.1:1"
	ln, err := daemon.ListenSocket(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = d.ServeSocket(ln) }()
	return &env{t: t, sock: sock}
}

// bridge connects as a bridge started by claudePID.
func (e *env) bridge(cwd string) {
	c, err := net.Dial("unix", e.sock)
	require.NoError(e.t, err)
	e.t.Cleanup(func() { _ = c.Close() })
	e.conn, e.sc = c, bufio.NewScanner(c)
	e.req(protocol.TypeHello, &protocol.Hello{Protocol: 1, WS: "ws_h", Cwd: cwd, BridgePPID: claudePID})
}

func (e *env) req(typ string, body any) protocol.Envelope {
	e.n++
	id := "m" + string(rune('0'+e.n))
	env, err := protocol.NewEnvelope(typ, id, "", "ws_h", body)
	require.NoError(e.t, err)
	b, _ := json.Marshal(env)
	_, err = e.conn.Write(append(b, '\n'))
	require.NoError(e.t, err)
	_ = e.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for e.sc.Scan() {
		var r protocol.Envelope
		require.NoError(e.t, json.Unmarshal(e.sc.Bytes(), &r))
		if r.Re == id {
			return r
		}
	}
	e.t.Fatalf("no reply to %s", typ)
	return protocol.Envelope{}
}

func run(sock string, event string, pids []int) (int, string) {
	var errOut bytes.Buffer
	code := Run(strings.NewReader(event), io.Discard, &errOut, sock, pids)
	return code, errOut.String()
}

func TestBlocksOnlyWhileSpecOpen(t *testing.T) {
	e := startDaemon(t)
	cwd := t.TempDir()
	spec := filepath.Join(cwd, "spec.md")
	require.NoError(t, os.WriteFile(spec, []byte("# Spec\n\nText.\n"), 0o644))
	ancestors := []int{1111, claudePID, 1}

	// Connected, but no spec shown yet: allow.
	e.bridge(cwd)
	code, _ := run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitAllow, code)

	// Spec shown: block, with the reason for Claude.
	e.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: spec})
	code, msg := run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitBlock, code)
	assert.Contains(t, msg, "ai-contextview `ask` tool")

	// Another Claude session (different ancestors): allow.
	code, _ = run(e.sock, askEvent, []int{2222, 3333})
	assert.Equal(t, ExitAllow, code)

	// Other tools are never blocked.
	code, _ = run(e.sock, `{"tool_name":"Bash"}`, ancestors)
	assert.Equal(t, ExitAllow, code)

	// Spec finished: allow again.
	e.req(protocol.TypeDone, &protocol.Done{})
	code, _ = run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitAllow, code)
}

func TestAllowsWithoutDaemonOrInput(t *testing.T) {
	code, msg := run("/tmp/does-not-exist-contextview.sock", askEvent, []int{claudePID})
	assert.Equal(t, ExitAllow, code)
	assert.Empty(t, msg)
	code, _ = run("/tmp/does-not-exist-contextview.sock", "not json", nil)
	assert.Equal(t, ExitAllow, code)
}

func TestAncestorPIDs(t *testing.T) {
	pids := identity.AncestorPIDs(16)
	require.NotEmpty(t, pids)
	assert.Equal(t, os.Getppid(), pids[0])
}

func TestBlocksInAskMode(t *testing.T) {
	e := startDaemon(t)
	cwd := t.TempDir()
	spec := filepath.Join(cwd, "spec.md")
	require.NoError(t, os.WriteFile(spec, []byte("# Spec\n\nText.\n"), 0o644))
	ancestors := []int{1111, claudePID, 1}
	e.bridge(cwd)

	// Ask mode with no spec: block, with the ask-mode reason.
	e.req(protocol.TypeAskMode, &protocol.AskMode{On: true})
	code, msg := run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitBlock, code)
	assert.Equal(t, ReasonAskMode+"\n", msg)

	// A spec open in ask mode: the spec reason.
	e.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: spec})
	code, msg = run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitBlock, code)
	assert.Equal(t, Reason+"\n", msg)

	// spec_done does not end ask mode.
	e.req(protocol.TypeDone, &protocol.Done{})
	code, msg = run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitBlock, code)
	assert.Equal(t, ReasonAskMode+"\n", msg)

	// Questions moved to the chat (ask_here): allow, until moved back.
	e.req(protocol.TypeAskHere, &protocol.AskHere{On: true})
	code, _ = run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitAllow, code)
	e.req(protocol.TypeAskHere, &protocol.AskHere{On: false})
	code, _ = run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitBlock, code)

	// Ask mode off: allow.
	e.req(protocol.TypeAskMode, &protocol.AskMode{On: false})
	code, _ = run(e.sock, askEvent, ancestors)
	assert.Equal(t, ExitAllow, code)
}

func TestCounts(t *testing.T) {
	home := "/home/u"
	yes := []string{
		"/r/docs/specs/SW-1.md", "/r/README.md", "/r/design.markdown",
		"/r/.claude/specs/draft.md", "/home/u/.claude/specs/p/draft.md", "/home/u/.claude/plans/plan.md",
	}
	no := []string{
		"/r/main.go", "/r/CLAUDE.md", "/r/sub/AGENTS.md", "/r/CHANGELOG.md", "/r/changelog.d/x.md",
		"/r/.agent/spec-review.md", "/r/.github/pull.md", "/r/.claude/notes.md",
		"/home/u/.claude/projects/p/memory/MEMORY.md",
	}
	for _, p := range yes {
		assert.True(t, Counts(p, home, "", nil), p)
	}
	for _, p := range no {
		assert.False(t, Counts(p, home, "", nil), p)
	}
	// Patterns replace the rules.
	pat := docPaths("/r", "docs/rfc/**, /abs/*.md")
	assert.Equal(t, []string{"/r/docs/rfc/**", "/abs/*.md"}, pat)
	assert.True(t, Counts("/r/docs/rfc/a/b.md", home, "/r", pat))
	assert.False(t, Counts("/r/docs/specs/SW-1.md", home, "/r", pat))
	assert.False(t, Counts("/r/docs/rfc/a.go", home, "/r", pat))
}

func TestWrittenDocBecomesWorkingDoc(t *testing.T) {
	e := startDaemon(t)
	cwd := t.TempDir()
	spec := filepath.Join(cwd, "docs", "specs", "SW-1.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(spec), 0o755))
	require.NoError(t, os.WriteFile(spec, []byte("# SW-1\n\n## Requirements\n\nR1.\n"), 0o644))
	notes := filepath.Join(cwd, "notes.md")
	require.NoError(t, os.WriteFile(notes, []byte("# Notes\n"), 0o644))
	ancestors := []int{1111, claudePID, 1}
	e.bridge(cwd)
	post := func(path string) string {
		ev, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Write", "cwd": cwd,
			"tool_input": map[string]string{"file_path": path}})
		var out bytes.Buffer
		code := Run(bytes.NewReader(ev), &out, io.Discard, e.sock, ancestors)
		assert.Equal(t, ExitAllow, code)
		return out.String()
	}

	// Not in ask mode: nothing happens.
	assert.Empty(t, post(spec))

	e.req(protocol.TypeAskMode, &protocol.AskMode{On: true})
	// Bookkeeping never counts.
	assert.Empty(t, post(filepath.Join(cwd, "CLAUDE.md")))
	// The first document becomes the working document, and Claude is told.
	out := post("docs/specs/SW-1.md")
	assert.Contains(t, out, `"hookEventName":"PostToolUse"`)
	assert.Contains(t, out, "now shows "+spec)
	// Later writes do not switch it.
	assert.Empty(t, post(notes))

	// The bridge is told, unasked.
	var shown protocol.DocShown
	for e.sc.Scan() {
		var env protocol.Envelope
		require.NoError(t, json.Unmarshal(e.sc.Bytes(), &env))
		if env.Type == protocol.TypeDocShown {
			require.NoError(t, env.Decode(&shown))
			break
		}
	}
	assert.Equal(t, spec, shown.RootAbs)

	// A skill start is accepted and never blocks.
	code, _ := run(e.sock, `{"hook_event_name":"PreToolUse","tool_name":"Skill","tool_input":{"skill":"harness-core:harness-elaborate-spec"}}`, ancestors)
	assert.Equal(t, ExitAllow, code)
}

func TestCountsInWorktree(t *testing.T) {
	wt := "/r/.claude/worktrees/fix-x"
	// A session in a desktop-app worktree: only the folders below it count.
	assert.True(t, Counts(wt+"/docs/specs/SW-1.md", "/home/u", wt, nil))
	assert.False(t, Counts(wt+"/.agent/review.md", "/home/u", wt, nil))
	// Seen from the main checkout, a worktree file is in a dot-folder.
	assert.False(t, Counts(wt+"/docs/specs/SW-1.md", "/home/u", "/r", nil))
	// Outside the session's folder, the whole path is checked.
	assert.False(t, Counts("/other/.hidden/a.md", "/home/u", wt, nil))
}

func TestOwnToolReportsSessionCwd(t *testing.T) {
	e := startDaemon(t)
	cwd := t.TempDir()
	e.bridge(cwd)
	wt := filepath.Join(cwd, ".claude", "worktrees", "x")
	ev, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__plugin_ai-contextview_ai-contextview__ask", "cwd": wt})
	code := Run(bytes.NewReader(ev), io.Discard, io.Discard, e.sock, []int{1111, claudePID, 1})
	assert.Equal(t, ExitAllow, code)

	// The bridge is told the session's folder, unasked.
	_ = e.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got protocol.SessionCwd
	for e.sc.Scan() {
		var env protocol.Envelope
		require.NoError(t, json.Unmarshal(e.sc.Bytes(), &env))
		if env.Type == protocol.TypeSessionCwd {
			require.NoError(t, env.Decode(&got))
			break
		}
	}
	assert.Equal(t, wt, got.Cwd)
}
