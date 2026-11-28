package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/testenv"
)

const spec = `# Retry spec

## Retry policy

All retries use **exponential backoff** capped at 30s.

## Errors

Errors are wrapped.
`

type harness struct {
	t    *testing.T
	d    *Daemon
	sock string
	http *httptest.Server
	host string
}

func newHarness(t *testing.T) *harness {
	dir := testenv.ShortTempDir(t, "svd")
	sock := filepath.Join(dir, "d.sock")

	d := New("test")
	t.Cleanup(d.RequestShutdown)
	ln, err := ListenSocket(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = d.ServeSocket(ln) }()

	srv := httptest.NewServer(nil)
	srv.Config.Handler = d.Handler()
	t.Cleanup(srv.Close)
	d.BaseURL = srv.URL
	u, _ := url.Parse(srv.URL)
	return &harness{t: t, d: d, sock: sock, http: srv, host: u.Host}
}

// bridge is a raw socket client.
type bridge struct {
	t  *testing.T
	c  net.Conn
	sc *bufio.Scanner
	ws string
	n  int
}

func (h *harness) bridge(ws, cwd string) *bridge {
	c, err := net.Dial("unix", h.sock)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = c.Close() })
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), protocol.MaxLine)
	b := &bridge{t: h.t, c: c, sc: sc, ws: ws}
	reply := b.req(protocol.TypeHello, &protocol.Hello{Protocol: 1, WS: ws, Cwd: cwd, ProjectName: filepath.Base(cwd)})
	require.Equal(h.t, protocol.TypeHelloAck, reply.Type)
	return b
}

func (b *bridge) send(typ string, body any) string {
	b.n++
	id := "m" + itoa(b.n)
	env, err := protocol.NewEnvelope(typ, id, "", b.ws, body)
	require.NoError(b.t, err)
	data, _ := json.Marshal(env)
	_, err = b.c.Write(append(data, '\n'))
	require.NoError(b.t, err)
	return id
}

func (b *bridge) read() protocol.Envelope {
	_ = b.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	require.True(b.t, b.sc.Scan(), "no message: %v", b.sc.Err())
	var env protocol.Envelope
	require.NoError(b.t, json.Unmarshal(b.sc.Bytes(), &env))
	return env
}

func (b *bridge) req(typ string, body any) protocol.Envelope {
	id := b.send(typ, body)
	for {
		env := b.read()
		if env.Re == id {
			return env
		}
	}
}

func writeSpec(t *testing.T) (string, string) {
	cwd := t.TempDir()
	p := filepath.Join(cwd, "spec.md")
	require.NoError(t, os.WriteFile(p, []byte(spec), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "code.go"), []byte("package x\n\nfunc A() {}\n\nfunc B() {}\n"), 0o644))
	return cwd, p
}

func (h *harness) do(method, path string, form url.Values, hdr map[string]string) *http.Response {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, h.http.URL+path, body)
	require.NoError(h.t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", h.http.URL)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if hh, ok := hdr["Host"]; ok {
		req.Host = hh
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func askBody(cwd, specPath, qs string) *protocol.Ask {
	return &protocol.Ask{QSID: qs, Title: "T", Questions: []protocol.AskQuestion{{
		ID: "q1", Text: "Reset?", Options: []protocol.Option{{Label: "Yes", Recommended: true}, {Label: "No"}},
		Docs: []protocol.ResolvedDocRef{{Path: specPath, Quote: "exponential backoff capped at 30s", Section: "Retry policy", Kind: "spec"}},
		Code: []protocol.ResolvedRef{{Path: filepath.Join(cwd, "code.go"), StartLine: 3, EndLine: 3, Source: "lines"}},
	}}}
}

func TestFullLoop(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)

	res := b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	require.Equal(t, protocol.TypeResult, res.Type)
	var ss protocol.SpecShowResult
	require.NoError(t, res.Decode(&ss))
	assert.Equal(t, "spec.md", ss.Primary)
	assert.Equal(t, []string{"Retry spec", "Retry policy", "Errors"}, ss.Sections)

	shownEnv := b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))
	require.Equal(t, protocol.TypeAskShown, shownEnv.Type)
	var shown protocol.AskShown
	require.NoError(t, shownEnv.Decode(&shown))
	assert.Equal(t, 0, shown.Viewers)
	require.Len(t, shown.Matches, 1)
	assert.Equal(t, protocol.MatchNormalized, shown.Matches[0].Match)

	// The doc fragment highlights the quote.
	resp := h.do(http.MethodGet, "/w/ws_A/doc?path=spec.md", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var doc map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	assert.Contains(t, doc["html"], `<mark class="q" data-ref="q1-0">`)

	// The panel marks the recommended option.
	h.d.mu.Lock()
	ev := h.d.askEvent(h.d.workspaces["ws_A"])
	h.d.mu.Unlock()
	assert.Contains(t, string(ev.Data), `opt recommended`)
	assert.Contains(t, string(ev.Data), `Recommended`)

	// Code fragment only for referenced files.
	resp = h.do(http.MethodGet, "/w/ws_A/code?path="+url.QueryEscape(filepath.Join(cwd, "code.go"))+"&start=3&end=3", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp = h.do(http.MethodGet, "/w/ws_A/code?path=/etc/passwd&start=1&end=1", nil, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Unanswered form is rejected.
	resp = h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Answer; first answer wins.
	resp = h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{"sel.q1": {"No"}, "notes.q1": {"cap resets daily"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp = h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{"sel.q1": {"Yes"}}, nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	env := b.read()
	require.Equal(t, protocol.TypeAskResult, env.Type)
	var ar protocol.AskResult
	require.NoError(t, env.Decode(&ar))
	assert.Equal(t, protocol.StatusAnswered, ar.Status)
	require.Len(t, ar.Answers, 1)
	assert.Equal(t, []string{"No"}, ar.Answers[0].Selected)
	require.NotNil(t, ar.Answers[0].Notes)
	assert.Equal(t, "cap resets daily", *ar.Answers[0].Notes)
}

// Every question offers Other, and an Other text alone answers it.
func TestOtherOnEveryQuestion(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))

	h.d.mu.Lock()
	ev := h.d.askEvent(h.d.workspaces["ws_A"])
	h.d.mu.Unlock()
	assert.Contains(t, string(ev.Data), `name=\"other.q1\"`)

	resp := h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{"other.q1": {"only after a full day"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	env := b.read()
	require.Equal(t, protocol.TypeAskResult, env.Type)
	var ar protocol.AskResult
	require.NoError(t, env.Decode(&ar))
	require.Len(t, ar.Answers, 1)
	assert.Empty(t, ar.Answers[0].Selected)
	require.NotNil(t, ar.Answers[0].Other)
	assert.Equal(t, "only after a full day", *ar.Answers[0].Other)
}

func TestReplaceAndCancel(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))

	// A second ask dismisses the first.
	id := b.send(protocol.TypeAsk, askBody(cwd, specPath, "qs_2"))
	var sawDismiss, sawShown bool
	for !sawDismiss || !sawShown {
		env := b.read()
		switch {
		case env.Type == protocol.TypeAskResult:
			var r protocol.AskResult
			require.NoError(t, env.Decode(&r))
			assert.Equal(t, "qs_1", r.QSID)
			assert.Equal(t, protocol.StatusDismissed, r.Status)
			sawDismiss = true
		case env.Re == id:
			sawShown = true
		}
	}

	res := b.req(protocol.TypeAskCancel, &protocol.AskCancel{QSID: "qs_2", Reason: "client_cancelled"})
	assert.Equal(t, protocol.TypeResult, res.Type)
	res = b.req(protocol.TypeAskCancel, &protocol.AskCancel{QSID: "qs_2"})
	assert.Equal(t, protocol.TypeError, res.Type)
	resp := h.do(http.MethodPost, "/w/ws_A/qs/qs_2/dismiss", url.Values{}, nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestIsolation(t *testing.T) {
	h := newHarness(t)
	cwdA, specA := writeSpec(t)
	cwdB, specB := writeSpec(t)
	a := h.bridge("ws_A", cwdA)
	b := h.bridge("ws_B", cwdB)
	a.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specA})
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specB})
	b.req(protocol.TypeAsk, askBody(cwdB, specB, "qs_b"))

	// A's page knows nothing about B's question or B's files.
	resp := h.do(http.MethodPost, "/w/ws_A/qs/qs_b/answer", url.Values{"sel.q1": {"No"}}, nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp = h.do(http.MethodGet, "/w/ws_A/code?path="+url.QueryEscape(filepath.Join(cwdB, "code.go")), nil, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	h.d.mu.Lock()
	assert.Nil(t, h.d.workspaces["ws_A"].Pending)
	assert.NotNil(t, h.d.workspaces["ws_B"].Pending)
	h.d.mu.Unlock()
}

func TestArchiveAndAdopt(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	a := h.bridge("ws_A", cwd)
	a.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	a.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))
	a.send(protocol.TypeBye, protocol.Bye{})

	require.Eventually(t, func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		w := h.d.workspaces["ws_A"]
		return w != nil && !w.Live
	}, 2*time.Second, 10*time.Millisecond)
	h.d.mu.Lock()
	assert.Nil(t, h.d.workspaces["ws_A"].Pending, "pending set closed as disconnected")
	assert.Len(t, h.d.workspaces["ws_A"].History, 1)
	h.d.mu.Unlock()

	// claude --resume: a new bridge in the same cwd adopts the workspace.
	c, err := net.Dial("unix", h.sock)
	require.NoError(t, err)
	defer c.Close()
	nb := &bridge{t: t, c: c, sc: bufio.NewScanner(c), ws: "ws_B"}
	env := nb.req(protocol.TypeHello, &protocol.Hello{Protocol: 1, WS: "ws_B", Cwd: cwd, ProjectName: "p"})
	var ack protocol.HelloAck
	require.NoError(t, env.Decode(&ack))
	assert.Equal(t, "ws_A", ack.AdoptedFrom)

	h.d.mu.Lock()
	w := h.d.workspaces["ws_B"]
	require.NotNil(t, w)
	assert.Len(t, w.History, 1)
	assert.NotNil(t, w.Root)
	h.d.mu.Unlock()

	// The old page URL redirects.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(h.http.URL + "/w/ws_A")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/w/ws_B", resp.Header.Get("Location"))
}

func TestSecurityChecks(t *testing.T) {
	h := newHarness(t)
	cwd, _ := writeSpec(t)
	h.bridge("ws_A", cwd)

	resp := h.do(http.MethodGet, "/", nil, map[string]string{"Host": "evil.example:80"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp = h.do(http.MethodGet, "/", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))

	resp = h.do(http.MethodPost, "/w/ws_A/qs/x/answer", url.Values{}, map[string]string{"Origin": "http://evil.example"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestSSEInitialState(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.http.URL+"/w/ws_A/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	seen := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() && len(seen) < 6 {
		if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			seen[name] = true
		}
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok && strings.Contains(data, `"qs_id":"qs_1"`) {
			assert.Contains(t, data, "Reset?")
		}
	}
	for _, n := range []string{"state", "artifacts", "doc", "code", "ask", "history"} {
		assert.True(t, seen[n], "missing initial %s event", n)
	}
	assert.Eventually(t, func() bool { return h.d.hub.Viewers("ws_A") == 1 }, time.Second, 10*time.Millisecond)
}

func TestChangeDetection(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})

	updated := strings.Replace(spec, "Errors are wrapped.", "Errors are wrapped with context.", 1)
	require.NoError(t, os.WriteFile(specPath, []byte(updated), 0o644))
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath, ChangedSections: []string{"Retry policy"}})

	h.d.mu.Lock()
	ids := h.d.workspaces["ws_A"].changedIDs("spec.md")
	h.d.mu.Unlock()
	assert.True(t, ids["retry-spec--errors"], "edited section detected")
	assert.True(t, ids["retry-spec--retry-policy"], "hinted section marked")
	assert.False(t, ids["retry-spec"])
}

func TestFolderRoot(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, "changes", "004")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "adr"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "proposal.md"), []byte(spec), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "adr", "002-cap.md"), []byte("# Decision\n\nthe cap resets at midnight UTC\n"), 0o644))
	b := h.bridge("ws_A", cwd)
	res := b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: root, IsDir: true})
	var ss protocol.SpecShowResult
	require.NoError(t, res.Decode(&ss))
	assert.Equal(t, "proposal.md", ss.Primary)
	require.Len(t, ss.Artifacts, 2)

	adr := filepath.Join(root, "adr", "002-cap.md")
	res = b.req(protocol.TypeFocus, &protocol.Focus{Docs: []protocol.ResolvedDocRef{{Path: adr, Quote: "the cap resets at midnight UTC", Kind: "adr"}}})
	var fr protocol.FocusResult
	require.NoError(t, res.Decode(&fr))
	require.Len(t, fr.Docs, 1)
	assert.Equal(t, "adr/002-cap.md", fr.Docs[0].Path)
	assert.Equal(t, protocol.MatchExact, fr.Docs[0].Match)

	// A new artifact is picked up by the refresh.
	require.NoError(t, os.WriteFile(filepath.Join(root, "adr", "003-new.md"), []byte("# New\n"), 0o644))
	h.d.refresh("ws_A")
	h.d.mu.Lock()
	assert.Len(t, h.d.workspaces["ws_A"].Root.Artifacts, 3)
	assert.True(t, h.d.workspaces["ws_A"].changedIDs("adr/003-new.md")["new"])
	h.d.mu.Unlock()
}

func TestOversizedLine(t *testing.T) {
	h := newHarness(t)
	c, err := net.Dial("unix", h.sock)
	require.NoError(t, err)
	defer c.Close()
	big := make([]byte, protocol.MaxLine+10)
	for i := range big {
		big[i] = 'x'
	}
	go func() { _, _ = c.Write(append(big, '\n')) }()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	sc := bufio.NewScanner(c)
	require.True(t, sc.Scan())
	var env protocol.Envelope
	require.NoError(t, json.Unmarshal(sc.Bytes(), &env))
	assert.Equal(t, protocol.TypeError, env.Type)
}

func TestSeveralMessagesInOneWrite(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	c, err := net.Dial("unix", h.sock)
	require.NoError(t, err)
	defer c.Close()
	e1, _ := protocol.NewEnvelope(protocol.TypeHello, "1", "", "ws_X", &protocol.Hello{Protocol: 1, WS: "ws_X", Cwd: cwd})
	e2, _ := protocol.NewEnvelope(protocol.TypeSpecShow, "2", "", "ws_X", &protocol.SpecShow{RootAbs: specPath})
	b1, _ := json.Marshal(e1)
	b2, _ := json.Marshal(e2)
	// Both messages in one write, the second split across two writes.
	_, _ = c.Write(append(append(b1, '\n'), b2[:10]...))
	time.Sleep(20 * time.Millisecond)
	_, _ = c.Write(append(b2[10:], '\n'))
	sc := bufio.NewScanner(c)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var res []string
	for len(res) < 2 && sc.Scan() {
		var env protocol.Envelope
		require.NoError(t, json.Unmarshal(sc.Bytes(), &env))
		res = append(res, env.Type+":"+env.Re)
	}
	assert.Equal(t, []string{"hello_ack:1", "result:2"}, res)
}

func TestDone(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))

	// A focus leaves a highlight and a code tab behind.
	b.req(protocol.TypeFocus, &protocol.Focus{
		Docs: []protocol.ResolvedDocRef{{Path: specPath, Quote: "Errors are wrapped.", Kind: "spec"}},
		Code: []protocol.ResolvedRef{{Path: filepath.Join(cwd, "code.go"), StartLine: 1, EndLine: 1, Source: "lines"}},
	})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1b"))

	// done withdraws the pending set and shows the summary.
	id := b.send(protocol.TypeDone, &protocol.Done{Summary: "Cap stays at **30s**."})
	var sawDismiss, sawResult bool
	for !sawDismiss || !sawResult {
		env := b.read()
		switch {
		case env.Type == protocol.TypeAskResult:
			var r protocol.AskResult
			require.NoError(t, env.Decode(&r))
			assert.Equal(t, protocol.StatusDismissed, r.Status)
			sawDismiss = true
		case env.Re == id:
			assert.Equal(t, protocol.TypeResult, env.Type)
			sawResult = true
		}
	}
	h.d.mu.Lock()
	w := h.d.workspaces["ws_A"]
	assert.Nil(t, w.Pending)
	assert.Empty(t, w.focus, "highlights cleared")
	assert.Empty(t, w.Code, "code tabs closed")
	doc := h.d.docPayload(w, "spec.md")
	assert.NotContains(t, doc["html"], "<mark")
	ev := h.d.askEvent(w)
	st := h.d.stateEvent(w)
	h.d.mu.Unlock()
	assert.Contains(t, string(ev.Data), `"done":true`)
	assert.Contains(t, string(ev.Data), `\u003cstrong\u003e30s`)
	assert.Contains(t, string(st.Data), `"done":true`)

	// A later ask reopens the session.
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_2"))
	h.d.mu.Lock()
	assert.Nil(t, h.d.workspaces["ws_A"].Done)
	h.d.mu.Unlock()

	res := b.req(protocol.TypeDone, &protocol.Done{Summary: strings.Repeat("x", protocol.MaxSummary+1)})
	assert.Equal(t, protocol.TypeError, res.Type)
}

func TestChanges(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})

	get := func(query string) map[string]any {
		resp := h.do(http.MethodGet, "/w/ws_A/changes?path=spec.md"+query, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}

	// Before any answer the base is the session start; nothing changed yet.
	out := get("")
	assert.Equal(t, "start", out["base"])
	assert.Equal(t, float64(0), out["count"])
	assert.Equal(t, false, out["has_answers"])

	// An edit before the first answer shows against the start.
	edit1 := strings.Replace(spec, "Errors are wrapped.", "Errors are wrapped with context.", 1)
	require.NoError(t, os.WriteFile(specPath, []byte(edit1), 0o644))
	h.d.refresh("ws_A")
	out = get("")
	assert.Equal(t, float64(1), out["count"])

	// The user answers: that is the new base.
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))
	resp := h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{"sel.q1": {"No"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	out = get("")
	assert.Equal(t, "answers", out["base"])
	assert.Equal(t, float64(0), out["count"], "nothing changed since the answers")

	// Claude applies the answer.
	edit2 := edit1 + "\n## Decisions\n\n- Do not reset the backoff.\n"
	require.NoError(t, os.WriteFile(specPath, []byte(edit2), 0o644))
	h.d.refresh("ws_A")
	out = get("")
	assert.Equal(t, float64(1), out["count"])
	assert.Contains(t, out["html"], `class="chg-add"`)
	assert.Contains(t, out["html"], "Do not reset the backoff.")
	assert.NotContains(t, out["html"], `class="chg-del"`, "the earlier edit is before the base")

	// Since session start, both edits show (adjacent, so one change).
	out = get("&base=start")
	assert.Equal(t, float64(1), out["count"])
	assert.Contains(t, out["html"], `<div class="chg-del"><p>Errors are wrapped.</p>`)
	assert.Contains(t, out["html"], `<div class="chg-add"><p>Errors are wrapped with context.</p>`)

	// Source mode is a line diff.
	out = get("&mode=source")
	assert.Contains(t, out["html"], `<span class="sd-add">+ - Do not reset the backoff.</span>`)

	// The doc payload carries the count for the tab label.
	h.d.mu.Lock()
	doc := h.d.docPayload(h.d.workspaces["ws_A"], "spec.md")
	h.d.mu.Unlock()
	assert.Equal(t, 1, doc["changes"])
	assert.Equal(t, "answers", doc["changes_base"])
}

func TestDecideLater(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	ask := askBody(cwd, specPath, "qs_1")
	q2 := ask.Questions[0]
	q2.ID, q2.Text = "q2", "Cap value?"
	ask.Questions = append(ask.Questions, q2)
	b.req(protocol.TypeAsk, ask)

	// q1 answered, q2 deferred with a note; a stray selection on q2 is ignored.
	resp := h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{
		"sel.q1": {"No"}, "later.q2": {"1"}, "sel.q2": {"Yes"}, "notes.q2": {"need a benchmark"},
	}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	env := b.read()
	require.Equal(t, protocol.TypeAskResult, env.Type)
	var r protocol.AskResult
	require.NoError(t, env.Decode(&r))
	assert.Equal(t, protocol.StatusAnswered, r.Status)
	assert.Equal(t, []string{"q2"}, r.Deferred)
	require.Len(t, r.Answers, 2)
	assert.False(t, r.Answers[0].Deferred)
	assert.True(t, r.Answers[1].Deferred)
	assert.Empty(t, r.Answers[1].Selected)
	require.NotNil(t, r.Answers[1].Notes)
	assert.Equal(t, "need a benchmark", *r.Answers[1].Notes)

	h.d.mu.Lock()
	hist := h.d.historyEvent(h.d.workspaces["ws_A"])
	h.d.mu.Unlock()
	assert.Contains(t, string(hist.Data), "decide later")
}

func TestPanelHasDecideLater(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))
	h.d.mu.Lock()
	ev := h.d.askEvent(h.d.workspaces["ws_A"])
	h.d.mu.Unlock()
	assert.Contains(t, string(ev.Data), `name=\"later.q1\"`)
	assert.NotContains(t, string(ev.Data), `keep.q1`, "the keep-open checkbox is gone")
}

func TestPanelHidesDecideLaterWhenOptionHasOne(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	ask := askBody(cwd, specPath, "qs_1")
	ask.Questions[0].Options = append(ask.Questions[0].Options, protocol.Option{Label: "Decide later", Description: "Park it"})
	b.req(protocol.TypeAsk, ask)
	h.d.mu.Lock()
	ev := h.d.askEvent(h.d.workspaces["ws_A"])
	h.d.mu.Unlock()
	assert.NotContains(t, string(ev.Data), `name=\"later.q1\"`)
	assert.Contains(t, string(ev.Data), `value=\"Decide later\"`)
}

func TestIsLaterLabel(t *testing.T) {
	for _, l := range []string{"Decide later", "decide later.", "Decide this later (park it)", "Defer", "Later"} {
		assert.True(t, isLaterLabel(l), l)
	}
	for _, l := range []string{"Defer writes to a queue", "Read later events", "Decide now"} {
		assert.False(t, isLaterLabel(l), l)
	}
}

func TestViewersMessage(t *testing.T) {
	h := newHarness(t)
	cwd, _ := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	res := b.req(protocol.TypeViewers, &protocol.Viewers{})
	require.Equal(t, protocol.TypeResult, res.Type)
	var v protocol.ViewersResult
	require.NoError(t, res.Decode(&v))
	assert.Equal(t, 0, v.Viewers)

	sub := h.d.hub.Subscribe("ws_A")
	t.Cleanup(func() { h.d.hub.Unsubscribe("ws_A", sub) })
	res = b.req(protocol.TypeViewers, &protocol.Viewers{})
	require.NoError(t, res.Decode(&v))
	assert.Equal(t, 1, v.Viewers)
}

func TestAskModeWithoutSpec(t *testing.T) {
	h := newHarness(t)
	cwd, _ := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	res := b.req(protocol.TypeAskMode, &protocol.AskMode{On: true})
	require.Equal(t, protocol.TypeResult, res.Type)

	// A question with no refs and no spec is shown.
	res = b.req(protocol.TypeAsk, &protocol.Ask{QSID: "qs_1", Questions: []protocol.AskQuestion{{
		ID: "q1", Text: "Tabs?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
	}}})
	require.Equal(t, protocol.TypeAskShown, res.Type)

	h.d.mu.Lock()
	w := h.d.workspaces["ws_A"]
	assert.True(t, w.AskMode)
	doc := h.d.docEvent(w, w.CurrentDoc)
	h.d.mu.Unlock()
	assert.Contains(t, string(doc.Data), "Ask mode is on")
}

func TestDocsOutsideSpecAndByLines(t *testing.T) {
	h := newHarness(t)
	cwd, _ := writeSpec(t)
	yaml := filepath.Join(cwd, "app.yaml")
	require.NoError(t, os.WriteFile(yaml, []byte("a: 1\nb: 2\nc: 3\n"), 0o644))
	notes := filepath.Join(cwd, "notes.md")
	require.NoError(t, os.WriteFile(notes, []byte("# Notes\n\nOne.\n\nTwo.\n\nThree.\n"), 0o644))
	b := h.bridge("ws_A", cwd)

	// No spec: documents by quote and by lines are shown.
	res := b.req(protocol.TypeAsk, &protocol.Ask{QSID: "qs_1", Questions: []protocol.AskQuestion{{
		ID: "q1", Text: "Ok?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
		Docs: []protocol.ResolvedDocRef{
			{Path: notes, StartLine: 5, EndLine: 5, Kind: protocol.KindOther},
			{Path: yaml, StartLine: 2, EndLine: 3, Kind: protocol.KindOther},
		},
	}}})
	require.Equal(t, protocol.TypeAskShown, res.Type)

	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	w := h.d.workspaces["ws_A"]
	md := h.d.docPayload(w, notes)
	assert.Equal(t, "notes.md", md["name"])
	notesSrc, err := os.ReadFile(notes)
	require.NoError(t, err)
	assert.Equal(t, lineCount(notesSrc), md["lines"])
	assert.Contains(t, md["html"], `class="q-section" data-ref="q1-0">Two.`, "the block on line 5 is tinted")
	assert.NotContains(t, md["html"], `data-ref="q1-0">One.`)
	src := h.d.docPayload(w, yaml)
	assert.Equal(t, true, src["source"])
	yamlSrc, err := os.ReadFile(yaml)
	require.NoError(t, err)
	assert.Equal(t, lineCount(yamlSrc), src["lines"])
	assert.Contains(t, src["html"], `class="doc-src"`)
	ev := h.d.askEvent(w)
	assert.Contains(t, string(ev.Data), `"doc_panel":true`)
}

func TestLineCount(t *testing.T) {
	assert.Equal(t, 0, lineCount(nil))
	assert.Equal(t, 1, lineCount([]byte("one")))
	assert.Equal(t, 1, lineCount([]byte("one\n")))
	assert.Equal(t, 2, lineCount([]byte("one\ntwo")))
	assert.Equal(t, 3, lineCount([]byte("one\n\nthree\n")))
}

func TestIndexShowsVersionOnce(t *testing.T) {
	h := newHarness(t)
	h.d.Version = "v1.2.3"
	resp := h.do(http.MethodGet, "/", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), ">v1.2.3<")
	assert.NotContains(t, string(body), "vv1.2.3")
}

func TestNoDocumentPill(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	ask := askBody(cwd, specPath, "qs_1")
	ask.Questions = append(ask.Questions, protocol.AskQuestion{
		ID: "q2", Text: "Ship?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
		Code: []protocol.ResolvedRef{{Path: filepath.Join(cwd, "code.go"), StartLine: 1, EndLine: 1, Source: "lines"}},
	})
	b.req(protocol.TypeAsk, ask)

	h.d.mu.Lock()
	ev := string(h.d.askEvent(h.d.workspaces["ws_A"]).Data)
	h.d.mu.Unlock()
	// Only the question with no document gets the pill.
	assert.Equal(t, 1, strings.Count(ev, `chip-nodoc`))
	assert.Contains(t, ev, `No document`)
}

func TestNoDocumentPillNeedsDocPanel(t *testing.T) {
	h := newHarness(t)
	cwd, _ := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeAskMode, &protocol.AskMode{On: true})
	b.req(protocol.TypeAsk, &protocol.Ask{QSID: "qs_1", Questions: []protocol.AskQuestion{{
		ID: "q1", Text: "Ship?", Options: []protocol.Option{{Label: "Yes"}, {Label: "No"}},
	}}})

	h.d.mu.Lock()
	ev := string(h.d.askEvent(h.d.workspaces["ws_A"]).Data)
	h.d.mu.Unlock()
	// No document panel: nothing to say.
	assert.NotContains(t, ev, `chip-nodoc`)
}

// Pages report when they are hidden and when someone uses them; the daemon
// passes both to the bridge.
func TestPresence(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	sub := h.d.hub.SubscribeAs("ws_A", "p1", true)
	defer h.d.hub.Unsubscribe("ws_A", sub)

	env := b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))
	var shown protocol.AskShown
	require.NoError(t, env.Decode(&shown))
	assert.Equal(t, 1, shown.Viewers)
	assert.Equal(t, 1, shown.Hidden, "the page connected hidden")

	res := h.d.viewers("ws_A")
	assert.True(t, res.Presence)
	assert.Equal(t, 1, res.Hidden)
	assert.False(t, res.Seen)

	// Shown again, then used.
	resp := h.do(http.MethodPost, "/w/ws_A/presence", url.Values{"c": {"p1"}, "hidden": {"0"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, 0, h.d.viewers("ws_A").Hidden)
	resp = h.do(http.MethodPost, "/w/ws_A/presence", url.Values{"c": {"p1"}, "active": {"1"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, h.d.viewers("ws_A").Seen)

	// A new question set is unseen until the page is used again.
	resp = h.do(http.MethodPost, "/w/ws_A/qs/qs_1/answer", url.Values{"sel.q1": {"No"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	b.read()
	time.Sleep(5 * time.Millisecond)
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_2"))
	assert.False(t, h.d.viewers("ws_A").Seen)

	resp = h.do(http.MethodPost, "/w/nope/presence", url.Values{"c": {"p1"}}, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ask_here moves the questions to the chat until someone uses a page again.
func TestAskHere(t *testing.T) {
	h := newHarness(t)
	cwd, specPath := writeSpec(t)
	b := h.bridge("ws_A", cwd)
	b.req(protocol.TypeSpecShow, &protocol.SpecShow{RootAbs: specPath})
	b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_1"))

	// The page was used after qs_1 was shown: an automatic move is refused.
	resp := h.do(http.MethodPost, "/w/ws_A/presence", url.Values{"c": {"p1"}, "active": {"1"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	var here protocol.AskHereResult
	require.NoError(t, envDecode(b.req(protocol.TypeAskHere, &protocol.AskHere{On: true, Auto: true}), &here))
	assert.False(t, here.InChat)

	// Asked for: the pending set closes with ask_in_chat.
	require.NoError(t, envDecode(b.req(protocol.TypeAskHere, &protocol.AskHere{On: true}), &here))
	assert.True(t, here.InChat)
	assert.Equal(t, "qs_1", here.Closed)
	h.d.mu.Lock()
	w := h.d.workspaces["ws_A"]
	assert.Nil(t, w.Pending)
	assert.Equal(t, statusAskInChat, w.History[len(w.History)-1].Status)
	h.d.mu.Unlock()

	// The next set is not shown: Claude asks it in the chat.
	var shown protocol.AskShown
	require.NoError(t, envDecode(b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_2")), &shown))
	assert.True(t, shown.InChat)
	h.d.mu.Lock()
	assert.Nil(t, w.Pending)
	assert.Len(t, w.History, 2)
	h.d.mu.Unlock()

	// Someone uses the page: questions come back to the viewer.
	resp = h.do(http.MethodPost, "/w/ws_A/presence", url.Values{"c": {"p1"}, "active": {"1"}}, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	shown = protocol.AskShown{}
	require.NoError(t, envDecode(b.req(protocol.TypeAsk, askBody(cwd, specPath, "qs_3")), &shown))
	assert.False(t, shown.InChat)
	h.d.mu.Lock()
	assert.NotNil(t, w.Pending)
	h.d.mu.Unlock()
}

func envDecode(env protocol.Envelope, v any) error { return env.Decode(v) }
