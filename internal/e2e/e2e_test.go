// Package e2e runs the whole path: an MCP client calls the ai-contextview tools,
// the bridge forwards to a real daemon, and the answer comes back over HTTP.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rrgmc/ai-contextview/internal/appconn"
	"github.com/rrgmc/ai-contextview/internal/daemon"
	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/resolve"
	"github.com/rrgmc/ai-contextview/internal/testenv"
	"github.com/rrgmc/ai-contextview/internal/tools"
)

const spec = `# Retry spec

## Retry policy

All retries use **exponential backoff** capped at 30s.
`

const code = `package retry

// Retrier retries.
type Retrier struct{ n int }

// Next returns the next delay.
func (r *Retrier) Next() int {
	r.n++
	return r.n
}
`

func TestEndToEnd(t *testing.T) {
	// Project with a spec and a Go package.
	cwd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module example.com/p\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, "retry"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "retry", "retry.go"), []byte(code), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "spec.md"), []byte(spec), 0o644))

	// Daemon.
	dir := testenv.ShortTempDir(t, "sve")
	sock := filepath.Join(dir, "d.sock")
	d := daemon.New("e2e")
	t.Cleanup(d.RequestShutdown)
	ln, err := daemon.ListenSocket(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = d.ServeSocket(ln) }()
	srv := httptest.NewServer(nil)
	srv.Config.Handler = d.Handler()
	t.Cleanup(srv.Close)
	d.BaseURL = srv.URL

	// Bridge behind an in-memory MCP transport.
	client := appconn.New(appconn.Options{Socket: sock, Hello: protocol.Hello{WS: "ws_e2e", Cwd: cwd, ProjectName: "p", BridgeVersion: "e2e"}})
	t.Cleanup(client.Close)
	var opened []string
	bridge := &tools.Bridge{Cwd: cwd, Daemon: client, Resolver: resolve.New(), AskWait: 5 * time.Second,
		Open: func(u string) { opened = append(opened, u) }}
	server := mcp.NewServer(&mcp.Implementation{Name: "ai-contextview", Version: "e2e"}, nil)
	tools.Register(server, bridge)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	_, err = server.Connect(ctx, st, nil)
	require.NoError(t, err)
	mc := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := mc.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	// The tool list carries generated schemas.
	list, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	assert.ElementsMatch(t, []string{"spec_show", "focus", "ask", "ask_wait", "ask_mode", "ask_here", "spec_done"}, names)

	call := func(name string, args any) *mcp.CallToolResult {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return res
	}
	text := func(r *mcp.CallToolResult) string {
		var b strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}

	res := call("spec_show", map[string]any{"path": "spec.md"})
	require.False(t, res.IsError, text(res))
	assert.Contains(t, text(res), srv.URL+"/w/ws_e2e")

	// A paraphrased quote is rejected before it reaches the UI.
	res = call("ask", map[string]any{"questions": []any{map[string]any{
		"id": "q1", "text": "Reset?", "options": []any{map[string]any{"label": "Yes"}, map[string]any{"label": "No"}},
		"spec_quote": "we back off more and more", "spec_section": "Retry policy",
	}}})
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "closest sections:")
	assert.Contains(t, text(res), "Retry policy")

	// An unknown symbol names the candidates.
	res = call("focus", map[string]any{"code_refs": []any{map[string]any{"path": "retry/retry.go", "symbol": "retry.(*Retrier).Nxt"}}})
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "Retrier.Next")

	// Answer from the "browser" while ask blocks.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/w/ws_e2e/qs/"+pendingQS(t, srv.URL)+"/answer",
				strings.NewReader(url.Values{"sel.q1": {"Keep growing"}, "notes.q1": {"cap resets daily"}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", srv.URL)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	res = call("ask", map[string]any{"title": "Retry semantics", "questions": []any{map[string]any{
		"id": "q1", "text": "Reset on partial success?",
		"options":    []any{map[string]any{"label": "Reset"}, map[string]any{"label": "Keep growing"}},
		"spec_quote": "retries use exponential backoff capped at 30s", "spec_section": "Retry policy",
		"code_refs": []any{map[string]any{"path": "retry/retry.go", "symbol": "retry.(*Retrier).Next"}},
	}}})
	require.False(t, res.IsError, text(res))
	var out protocol.AskOutput
	require.NoError(t, json.Unmarshal([]byte(text(res)), &out))
	assert.Equal(t, protocol.StatusAnswered, out.Status)
	require.Len(t, out.Answers, 1)
	assert.Equal(t, []string{"Keep growing"}, out.Answers[0].Selected)
	// Whether the URL was opened depends on whether the test's own SSE reader
	// was connected when the question was shown; the tools tests cover it.
	_ = opened
}

// pendingQS scrapes the pending question set id from the SSE ask event.
func pendingQS(t *testing.T, base string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/w/ws_e2e/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "none"
	}
	defer resp.Body.Close()
	buf := make([]byte, 1<<20)
	var all strings.Builder
	for {
		n, err := resp.Body.Read(buf)
		all.Write(buf[:n])
		s := all.String()
		if i := strings.Index(s, `"qs_id":"qs_`); i >= 0 {
			rest := s[i+len(`"qs_id":"`):]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				return rest[:j]
			}
		}
		if strings.Contains(s, "event: history") || err != nil {
			return "none"
		}
	}
}
