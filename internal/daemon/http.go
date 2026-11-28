package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/render"
	"github.com/rrgmc/ai-contextview/internal/specsrc"
)

// ListenHTTP listens on addr, trying the next 9 ports when it is taken.
func ListenHTTP(addr string) (net.Listener, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < 10; i++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port+i)))
		if err == nil {
			return ln, nil
		}
		lastErr = err
		if port == 0 {
			break
		}
	}
	return nil, lastErr
}

// Handler returns the HTTP handler. BaseURL must be set.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.handleIndex)
	mux.HandleFunc("GET /events", d.handleIndexEvents)
	mux.HandleFunc("GET /w/{ws}", d.handlePage)
	mux.HandleFunc("GET /w/{ws}/events", d.handleEvents)
	mux.HandleFunc("GET /w/{ws}/doc", d.handleDoc)
	mux.HandleFunc("GET /w/{ws}/code", d.handleCode)
	mux.HandleFunc("GET /w/{ws}/changes", d.handleChanges)
	mux.HandleFunc("POST /w/{ws}/qs/{qs}/{action}", d.handleAnswer)
	mux.HandleFunc("POST /w/{ws}/close", d.handleClose)
	mux.HandleFunc("POST /w/{ws}/open", d.handleOpen)
	mux.HandleFunc("POST /w/{ws}/presence", d.handlePresence)
	mux.HandleFunc("GET /static/chroma.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(chromaCSS))
	})
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	return d.guard(mux)
}

var chromaCSS = render.CodeCSS()

// guard rejects foreign Host headers (DNS rebinding) and POSTs from other
// origins (CSRF). No CORS headers are ever sent.
func (d *Daemon) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		base := d.BaseURL
		d.mu.Unlock()
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
		okHost := r.Host == "127.0.0.1:"+port || r.Host == "localhost:"+port
		if !okHost {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			o := r.Header.Get("Origin")
			if o != "http://127.0.0.1:"+port && o != "http://localhost:"+port {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (d *Daemon) handleIndex(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	items := d.indexItems()
	d.mu.Unlock()
	d.touch()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(execTemplate("index", map[string]any{"Items": items, "Version": d.Version})))
}

func (d *Daemon) handlePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("ws")
	d.mu.Lock()
	ws, real := d.lookup(id)
	var data map[string]any
	if ws != nil {
		data = map[string]any{"ID": ws.ID, "Project": ws.Project, "Version": d.Version}
	}
	d.mu.Unlock()
	if ws == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	if real != id {
		http.Redirect(w, r, "/w/"+real, http.StatusFound)
		return
	}
	d.touch()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(execTemplate("page", data)))
}

func sseHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return fl, true
}

func writeEvent(w http.ResponseWriter, ev Event) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
	return err
}

func (d *Daemon) stream(w http.ResponseWriter, r *http.Request, key string, initial func() []Event) {
	fl, ok := sseHeaders(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	sub := d.hub.SubscribeAs(key, q.Get("c"), q.Get("h") == "1")
	defer func() {
		d.hub.Unsubscribe(key, sub)
		d.touch()
		if key != indexKey {
			d.publishIndex()
		}
	}()
	if key != indexKey {
		d.publishIndex()
	}
	// Last-Event-ID is ignored: the current state is simply sent again.
	for _, ev := range initial() {
		if writeEvent(w, ev) != nil {
			return
		}
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev := <-sub.ch:
			if writeEvent(w, ev) != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		case <-d.shutdown:
			return
		}
	}
}

func (d *Daemon) handleIndexEvents(w http.ResponseWriter, r *http.Request) {
	d.stream(w, r, indexKey, func() []Event { return nil })
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("ws")
	d.mu.Lock()
	ws, real := d.lookup(id)
	d.mu.Unlock()
	if ws == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	if real != id {
		fl, ok := sseHeaders(w)
		if ok {
			_ = writeEvent(w, newEvent("redirect", map[string]string{"url": "/w/" + real}))
			fl.Flush()
		}
		return
	}
	d.stream(w, r, id, func() []Event {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.snapshot(ws)
	})
}

// handlePresence records what a page reports about itself: hidden=1 or 0
// when it is hidden or shown again, active=1 when someone used it.
func (d *Daemon) handlePresence(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	ws, real := d.lookup(r.PathValue("ws"))
	if ws != nil && r.Form.Get("active") == "1" {
		ws.SeenAt = time.Now()
		if ws.InChat {
			// The user is back at the viewer: questions come here again.
			ws.InChat = false
			d.hub.Publish(ws.ID, d.stateEvent(ws))
		}
	}
	d.mu.Unlock()
	if ws == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	if h := r.Form.Get("hidden"); h == "0" || h == "1" {
		d.hub.SetHidden(real, r.Form.Get("c"), h == "1")
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (d *Daemon) handleDoc(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, _ := d.lookup(r.PathValue("ws"))
	if ws == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	rel := r.URL.Query().Get("path")
	if _, ok := ws.docAbs(rel); !ok {
		http.Error(w, "unknown document", http.StatusNotFound)
		return
	}
	writeJSON(w, d.docPayload(ws, rel))
}

func (d *Daemon) handleCode(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, _ := d.lookup(r.PathValue("ws"))
	if ws == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	path := q.Get("path")
	if !ws.codeAllowed[path] {
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
	start, _ := strconv.Atoi(q.Get("start"))
	end, _ := strconv.Atoi(q.Get("end"))
	var tab *CodeTab
	for _, t := range ws.Code {
		if t.Path == path {
			tab = t
		}
	}
	if tab == nil {
		tab = &CodeTab{Path: path}
		ws.addCode(protocol.ResolvedRef{Path: path, StartLine: start, EndLine: end, Source: "lines"})
	}
	writeJSON(w, d.codePayload(ws, tab, start, end))
}

// handleChanges renders the diff of a doc against a base: "answers" (the
// doc when the user last replied) or "start" (when first seen). mode is
// "rendered" (block diff, default) or "source" (line diff).
func (d *Daemon) handleChanges(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, _ := d.lookup(r.PathValue("ws"))
	if ws == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	rel := q.Get("path")
	abs, ok := ws.docAbs(rel)
	if !ok {
		http.Error(w, "unknown document", http.StatusNotFound)
		return
	}
	src, err := specsrc.ReadPrepared(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	base := q.Get("base")
	if base != baseAnswers && base != baseStart {
		base = ws.defaultBase()
	}
	if base == baseAnswers && ws.AnswersAt.IsZero() {
		base = baseStart
	}
	old := ws.baseFor(rel, base)
	var html string
	var count int
	if q.Get("mode") == "source" {
		html, count = render.SourceChangesHTML(old, src)
	} else {
		edits := render.DiffBlocks(old, src)
		html, count = render.ChangesHTML(edits), render.Hunks(edits)
	}
	since := ws.Started
	if base == baseAnswers {
		since = ws.AnswersAt
	}
	writeJSON(w, map[string]any{
		"path": rel, "base": base, "since": since.Format("15:04"), "html": html, "count": count,
		"has_answers": !ws.AnswersAt.IsZero(),
	})
}

const maxForm = 1 << 20

func (d *Daemon) handleAnswer(w http.ResponseWriter, r *http.Request) {
	id, qsID, action := r.PathValue("ws"), r.PathValue("qs"), r.PathValue("action")
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var status string
	switch action {
	case "answer":
		status = protocol.StatusAnswered
	case "in-chat":
		status = protocol.StatusInChat
	case "dismiss":
		status = protocol.StatusDismissed
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}

	d.mu.Lock()
	ws, _ := d.lookup(id)
	var qs *questionSet
	if ws != nil && ws.Pending != nil && ws.Pending.Ask.QSID == qsID {
		qs = ws.Pending
	}
	d.mu.Unlock()
	if qs == nil {
		http.Error(w, "already answered", http.StatusConflict)
		return
	}

	var answers []protocol.Answer
	var keep, deferred []string
	var problems []string
	for _, q := range qs.Ask.Questions {
		if r.Form.Get("keep."+q.ID) != "" && status != protocol.StatusAnswered {
			keep = append(keep, q.ID)
		}
		if status != protocol.StatusAnswered {
			continue
		}
		a := protocol.Answer{ID: q.ID, Selected: []string{}}
		if r.Form.Get("later."+q.ID) != "" {
			// "Decide later": no selection; notes still go to Claude.
			a.Deferred = true
			if notes := strings.TrimSpace(r.Form.Get("notes." + q.ID)); notes != "" {
				a.Notes = &notes
			}
			deferred = append(deferred, q.ID)
			answers = append(answers, a)
			continue
		}
		valid := map[string]bool{}
		for _, o := range q.Options {
			valid[o.Label] = true
		}
		for _, v := range r.Form["sel."+q.ID] {
			if valid[v] {
				a.Selected = append(a.Selected, v)
			}
		}
		if !q.MultiSelect && len(a.Selected) > 1 {
			a.Selected = a.Selected[:1]
		}
		if other := strings.TrimSpace(r.Form.Get("other." + q.ID)); other != "" {
			a.Other = &other
		}
		if notes := strings.TrimSpace(r.Form.Get("notes." + q.ID)); notes != "" {
			a.Notes = &notes
		}
		if len(a.Selected) == 0 && a.Other == nil {
			problems = append(problems, q.ID)
		}
		answers = append(answers, a)
	}
	if len(problems) > 0 {
		http.Error(w, "unanswered: "+strings.Join(problems, ", "), http.StatusBadRequest)
		return
	}
	err := d.Answer(id, qsID, status, answers, keep, deferred)
	switch {
	case errors.Is(err, ErrNotPending):
		http.Error(w, "already answered", http.StatusConflict)
	case errors.Is(err, ErrGone):
		http.Error(w, err.Error(), http.StatusGone)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (d *Daemon) handleClose(w http.ResponseWriter, r *http.Request) {
	if err := d.Close(r.PathValue("ws")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
