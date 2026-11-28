package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/specsrc"
)

// Conn is one bridge connection. Its workspace is set by hello.
type Conn struct {
	d    *Daemon
	c    net.Conn
	mu   sync.Mutex
	ws   string
	gone bool
}

// send writes one message; errors mark the connection gone.
func (c *Conn) send(typ, re string, body any) {
	env, err := protocol.NewEnvelope(typ, "", re, "", body)
	if err != nil {
		return
	}
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return
	}
	if _, err := c.c.Write(append(b, '\n')); err != nil {
		c.gone = true
	}
}

func (c *Conn) sendError(re string, err error) {
	var pe *protocol.ErrorBody
	if errors.As(err, &pe) {
		c.send(protocol.TypeError, re, pe)
		return
	}
	code := specsrc.Code(err)
	if code == protocol.CodeInternal {
		var se *specsrc.Error
		if !errors.As(err, &se) {
			code = protocol.CodeBadRequest
		}
	}
	c.send(protocol.TypeError, re, &protocol.ErrorBody{Code: code, Message: err.Error()})
}

// send sends a message to the workspace's bridge, if connected.
func (w *Workspace) send(typ, re string, body any) {
	if w.conn != nil {
		w.conn.send(typ, re, body)
	}
}

// ListenSocket listens on the Unix socket with mode 0600, removing a stale
// socket file first. The caller must have checked that no daemon answers.
func ListenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// ServeSocket accepts bridge connections until the listener closes.
func (d *Daemon) ServeSocket(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go d.handleConn(&Conn{d: d, c: c})
	}
}

func (d *Daemon) handleConn(c *Conn) {
	defer func() {
		_ = c.c.Close()
		if c.ws != "" {
			d.archive(c.ws, c)
		}
	}()
	sc := bufio.NewScanner(c.c)
	sc.Buffer(make([]byte, 64*1024), protocol.MaxLine)
	for sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			c.sendError("", &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: "bad json: " + err.Error()})
			continue
		}
		if err := env.Validate(); err != nil {
			c.sendError(env.ID, &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: err.Error()})
			continue
		}
		if !d.dispatch(c, env) {
			return
		}
	}
	if err := sc.Err(); err != nil {
		// Oversized line or read error: tell the bridge, then drop it.
		c.sendError("", &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: err.Error()})
	}
}

func decode[T protocol.Validator](env protocol.Envelope, v T) error {
	if err := env.Decode(v); err != nil {
		return &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	if err := v.Validate(); err != nil {
		return &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	return nil
}

// dispatch handles one message; it returns false to close the connection.
func (d *Daemon) dispatch(c *Conn, env protocol.Envelope) bool {
	switch env.Type {
	case protocol.TypePing:
		d.mu.Lock()
		url := d.BaseURL
		d.mu.Unlock()
		c.send(protocol.TypeResult, env.ID, &protocol.PingResult{DaemonVersion: d.Version, URL: url, PID: os.Getpid()})
		return true
	case protocol.TypeActive:
		var b protocol.Active
		if err := env.Decode(&b); err != nil {
			c.sendError(env.ID, &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: err.Error()})
			return true
		}
		c.send(protocol.TypeResult, env.ID, d.active(b.PIDs, b.Cwd))
		return true
	case protocol.TypeDocWritten:
		var b protocol.DocWritten
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, d.docWritten(&b))
		return true
	case protocol.TypeSessionCwd:
		var b protocol.SessionCwd
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		d.sessionCwd(&b)
		c.send(protocol.TypeResult, env.ID, map[string]any{})
		return true
	case protocol.TypeSkillStarted:
		var b protocol.SkillStarted
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		d.skillStarted(&b)
		c.send(protocol.TypeResult, env.ID, map[string]any{})
		return true
	case protocol.TypeShutdown:
		c.send(protocol.TypeResult, env.ID, map[string]any{})
		d.RequestShutdown()
		return false
	case protocol.TypeHello:
		var h protocol.Hello
		if err := decode(env, &h); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		ack, err := d.hello(c, &h)
		if err != nil {
			c.sendError(env.ID, &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: err.Error()})
			return false
		}
		c.ws = h.WS
		c.send(protocol.TypeHelloAck, env.ID, ack)
		return true
	case protocol.TypeBye:
		return false
	}

	if c.ws == "" {
		c.sendError(env.ID, &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: "send hello first"})
		return true
	}
	switch env.Type {
	case protocol.TypeCwd:
		c.send(protocol.TypeResult, env.ID, d.cwd(c.ws))
	case protocol.TypeSpecShow:
		var b protocol.SpecShow
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		res, err := d.specShow(c.ws, &b)
		if err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, res)
	case protocol.TypeFocus:
		var b protocol.Focus
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		res, err := d.focus(c.ws, &b)
		if err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, res)
	case protocol.TypeAsk:
		var b protocol.Ask
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		res, err := d.ask(c.ws, &b)
		if err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeAskShown, env.ID, res)
	case protocol.TypeDone:
		var b protocol.Done
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		if err := d.done(c.ws, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, map[string]any{})
	case protocol.TypeAskCancel:
		var b protocol.AskCancel
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		if err := d.cancel(c.ws, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, map[string]any{})
	case protocol.TypeAskMode:
		var b protocol.AskMode
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		res, err := d.askMode(c.ws, &b)
		if err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, res)
	case protocol.TypeAskHere:
		var b protocol.AskHere
		if err := decode(env, &b); err != nil {
			c.sendError(env.ID, err)
			return true
		}
		res, err := d.askHere(c.ws, &b)
		if err != nil {
			c.sendError(env.ID, err)
			return true
		}
		c.send(protocol.TypeResult, env.ID, res)
	case protocol.TypeViewers:
		c.send(protocol.TypeResult, env.ID, d.viewers(c.ws))
	default:
		c.sendError(env.ID, &protocol.ErrorBody{Code: protocol.CodeBadRequest, Message: "unknown message type " + env.Type})
	}
	return true
}
