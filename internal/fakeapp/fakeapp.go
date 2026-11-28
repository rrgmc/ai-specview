// Package fakeapp is a test double for the viewer daemon. It speaks the socket
// protocol, prints every message, and answers questions either from a hook
// (tests) or from the terminal (`ai-contextview fakeapp`).
package fakeapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/specsrc"
)

// Server is a fake daemon.
type Server struct {
	Socket  string
	BaseURL string
	Viewers int
	// Hidden, Seen and Presence are reported as a presence-tracking daemon
	// reports them (SetPresence); without Presence they are not sent.
	Hidden   int
	Seen     bool
	Presence bool
	// InChat is set by ask_here: asks are answered with in_chat.
	InChat bool
	// SessionCwd is the session folder the server answers cwd with.
	SessionCwd string
	// OnAsk returns the answer to a question set, or nil to leave it pending.
	// It runs in its own goroutine.
	OnAsk func(ws string, ask *protocol.Ask) *protocol.AskResult
	Out   io.Writer

	mu       sync.Mutex
	ln       net.Listener
	conns    map[string]*conn
	received []protocol.Envelope
}

type conn struct {
	c  net.Conn
	mu sync.Mutex
}

func (c *conn) send(env protocol.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.c.Write(append(b, '\n'))
	return err
}

// Listen opens the socket.
func (s *Server) Listen() error {
	if s.BaseURL == "" {
		s.BaseURL = "http://127.0.0.1:0"
	}
	if s.Out == nil {
		s.Out = io.Discard
	}
	s.conns = map[string]*conn{}
	_ = os.MkdirAll(filepath.Dir(s.Socket), 0o700)
	_ = os.Remove(s.Socket)
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Serve accepts connections until ctx ends or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(&conn{c: c})
	}
}

// Close stops the listener and drops every connection.
func (s *Server) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.c.Close()
	}
	s.mu.Unlock()
}

// Received returns every message received so far.
func (s *Server) Received() []protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.Envelope(nil), s.received...)
}

// Answer sends an ask_result to a workspace.
func (s *Server) Answer(ws string, r *protocol.AskResult) error {
	s.mu.Lock()
	c := s.conns[ws]
	s.mu.Unlock()
	if c == nil {
		return fmt.Errorf("no connection for %s", ws)
	}
	env, err := protocol.NewEnvelope(protocol.TypeAskResult, "", "", "", r)
	if err != nil {
		return err
	}
	return c.send(env)
}

func (s *Server) reply(c *conn, re, typ string, body any) {
	env, err := protocol.NewEnvelope(typ, "", re, "", body)
	if err == nil {
		_ = c.send(env)
	}
}

func (s *Server) handle(c *conn) {
	defer c.c.Close()
	sc := bufio.NewScanner(c.c)
	sc.Buffer(make([]byte, 64*1024), protocol.MaxLine)
	var ws string
	for sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		s.mu.Lock()
		s.received = append(s.received, env)
		s.mu.Unlock()
		fmt.Fprintf(s.Out, "← %s\n", sc.Text())
		switch env.Type {
		case protocol.TypeHello:
			var h protocol.Hello
			_ = env.Decode(&h)
			ws = h.WS
			s.mu.Lock()
			s.conns[ws] = c
			s.mu.Unlock()
			s.reply(c, env.ID, protocol.TypeHelloAck, &protocol.HelloAck{DaemonVersion: "fake", URL: s.BaseURL + "/w/" + ws})
		case protocol.TypeSpecShow:
			var b protocol.SpecShow
			_ = env.Decode(&b)
			root, err := specsrc.LoadRoot(b.RootAbs, b.Primary)
			if err != nil {
				s.reply(c, env.ID, protocol.TypeError, &protocol.ErrorBody{Code: specsrc.Code(err), Message: err.Error()})
				continue
			}
			s.reply(c, env.ID, protocol.TypeResult, &protocol.SpecShowResult{
				Primary: root.Primary, Sections: root.Artifact(root.Primary).Sections, Artifacts: root.Artifacts,
			})
		case protocol.TypeFocus:
			var b protocol.Focus
			_ = env.Decode(&b)
			res := protocol.FocusResult{Docs: []protocol.DocMatch{}}
			for _, d := range b.Docs {
				res.Docs = append(res.Docs, protocol.DocMatch{Path: d.Path, Match: match(d)})
			}
			s.reply(c, env.ID, protocol.TypeResult, &res)
		case protocol.TypeAsk:
			var b protocol.Ask
			_ = env.Decode(&b)
			shown := protocol.AskShown{QSID: b.QSID, Viewers: s.viewers(), Matches: []protocol.QuestionMatch{}}
			shown.Hidden = s.viewersResult().Hidden
			s.mu.Lock()
			shown.InChat = s.InChat
			s.mu.Unlock()
			if shown.InChat {
				s.reply(c, env.ID, protocol.TypeAskShown, &shown)
				continue
			}
			for _, q := range b.Questions {
				for i, d := range q.Docs {
					shown.Matches = append(shown.Matches, protocol.QuestionMatch{ID: q.ID, Doc: i, Match: match(d)})
				}
			}
			s.reply(c, env.ID, protocol.TypeAskShown, &shown)
			if s.OnAsk != nil {
				wsID := ws
				go func() {
					if r := s.OnAsk(wsID, &b); r != nil {
						_ = s.Answer(wsID, r)
					}
				}()
			}
		case protocol.TypeCwd:
			s.mu.Lock()
			res := &protocol.CwdResult{Cwd: s.SessionCwd}
			s.mu.Unlock()
			s.reply(c, env.ID, protocol.TypeResult, res)
		case protocol.TypeViewers:
			s.reply(c, env.ID, protocol.TypeResult, s.viewersResult())
		case protocol.TypeAskHere:
			var b protocol.AskHere
			_ = env.Decode(&b)
			s.mu.Lock()
			if !b.Auto || !s.Seen {
				s.InChat = b.On
			}
			res := &protocol.AskHereResult{InChat: s.InChat}
			s.mu.Unlock()
			s.reply(c, env.ID, protocol.TypeResult, res)
		case protocol.TypeAskCancel, protocol.TypePing, protocol.TypeDone, protocol.TypeAskMode:
			s.reply(c, env.ID, protocol.TypeResult, map[string]any{})
		case protocol.TypeBye:
			return
		default:
			s.reply(c, env.ID, protocol.TypeError, &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: "unknown type " + env.Type})
		}
	}
}

// SetViewers changes the number of open pages the server reports.
func (s *Server) SetViewers(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Viewers = n
}

// SetSessionCwd changes the session folder the server answers cwd with.
func (s *Server) SetSessionCwd(cwd string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SessionCwd = cwd
}

// SetPresence makes the server track presence: hidden of its viewers are
// hidden, and seen says whether someone used a page.
func (s *Server) SetPresence(hidden int, seen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Hidden, s.Seen, s.Presence = hidden, seen, true
}

func (s *Server) viewersResult() *protocol.ViewersResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &protocol.ViewersResult{Viewers: s.Viewers}
	if s.Presence {
		res.Hidden, res.Seen, res.Presence = s.Hidden, s.Seen, true
	}
	return res
}

func (s *Server) viewers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Viewers
}

func match(d protocol.ResolvedDocRef) protocol.Match {
	if d.StartLine > 0 {
		return protocol.MatchExact
	}
	src, err := specsrc.ReadPrepared(d.Path)
	if err != nil {
		return protocol.MatchNone
	}
	return specsrc.Match(src, d.Quote, d.Section).Quality
}

// TerminalAnswerer returns an OnAsk hook that prompts on the terminal.
func TerminalAnswerer(in io.Reader, out io.Writer) func(string, *protocol.Ask) *protocol.AskResult {
	var mu sync.Mutex
	r := bufio.NewReader(in)
	return func(ws string, ask *protocol.Ask) *protocol.AskResult {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(out, "\n=== question set %s (%s) %s\n", ask.QSID, ws, ask.Title)
		res := &protocol.AskResult{QSID: ask.QSID, Status: protocol.StatusAnswered}
		for _, q := range ask.Questions {
			fmt.Fprintf(out, "\n[%s] %s\n", q.ID, q.Text)
			for _, d := range q.Docs {
				fmt.Fprintf(out, "   doc: %s § %s: %q\n", d.Path, d.Section, d.Quote)
			}
			for _, c := range q.Code {
				fmt.Fprintf(out, "   code: %s:%d-%d %s\n", c.Path, c.StartLine, c.EndLine, c.Symbol)
			}
			for i, o := range q.Options {
				fmt.Fprintf(out, "   %d) %s  %s\n", i+1, o.Label, o.Description)
			}
			fmt.Fprint(out, "answer (numbers separated by commas, text for Other, 'l' = decide later, 'c' = in chat, 'd' = dismiss): ")
			line, err := r.ReadString('\n')
			if err != nil {
				return nil
			}
			line = strings.TrimSpace(line)
			switch line {
			case "c":
				return &protocol.AskResult{QSID: ask.QSID, Status: protocol.StatusInChat}
			case "d":
				return &protocol.AskResult{QSID: ask.QSID, Status: protocol.StatusDismissed}
			}
			a := protocol.Answer{ID: q.ID, Selected: []string{}}
			if line == "l" {
				a.Deferred = true
				res.Deferred = append(res.Deferred, q.ID)
				res.Answers = append(res.Answers, a)
				continue
			}
			for _, f := range strings.Split(line, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n >= 1 && n <= len(q.Options) {
					a.Selected = append(a.Selected, q.Options[n-1].Label)
				}
			}
			if len(a.Selected) == 0 && line != "" {
				a.Other = &line
			}
			res.Answers = append(res.Answers, a)
		}
		return res
	}
}
