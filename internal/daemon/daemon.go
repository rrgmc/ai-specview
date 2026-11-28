// Package daemon is `ai-contextview serve`: the socket server for bridges, the
// workspaces, file watching, and the web viewer.
package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/render"
	"github.com/rrgmc/ai-contextview/internal/specsrc"
)

// Daemon holds every workspace.
type Daemon struct {
	Version string
	BaseURL string
	Logf    func(format string, args ...any)

	mu         sync.Mutex
	workspaces map[string]*Workspace
	// pendingCwd keeps a session folder the hook reported before the
	// session's bridge connected, by the hook's ancestor process ids, so the
	// workspace gets it at hello.
	pendingCwd map[int]pendingCwd
	aliases    map[string]string
	hub        *Hub
	watcher    *fsnotify.Watcher
	watched    map[string]bool
	timers     map[string]*time.Timer
	lastActive time.Time
	shutdown   chan struct{}
	shutOnce   sync.Once
	// openEditor runs an editor command; nil runs it (tests replace it).
	openEditor func(args []string) error
}

// New creates a daemon. BaseURL is set once the HTTP listener is up.
func New(version string) *Daemon {
	d := &Daemon{
		Version:    version,
		Logf:       func(string, ...any) {},
		workspaces: map[string]*Workspace{},
		pendingCwd: map[int]pendingCwd{},
		aliases:    map[string]string{},
		hub:        newHub(),
		watched:    map[string]bool{},
		timers:     map[string]*time.Timer{},
		lastActive: time.Now(),
		shutdown:   make(chan struct{}),
	}
	if w, err := fsnotify.NewWatcher(); err == nil {
		d.watcher = w
		go d.watchLoop()
	}
	go d.pollLoop()
	return d
}

// Shutdown is closed when the daemon should exit.
func (d *Daemon) Shutdown() <-chan struct{} { return d.shutdown }

// RequestShutdown asks the daemon to exit.
func (d *Daemon) RequestShutdown() { d.shutOnce.Do(func() { close(d.shutdown) }) }

func (d *Daemon) wsURL(id string) string { return d.BaseURL + "/w/" + id }

// lookup returns a workspace by id, following adoption aliases.
func (d *Daemon) lookup(id string) (*Workspace, string) {
	for i := 0; i < 10; i++ {
		if w := d.workspaces[id]; w != nil {
			return w, id
		}
		next, ok := d.aliases[id]
		if !ok {
			return nil, ""
		}
		id = next
	}
	return nil, ""
}

// --------------------------------------------------------------- messages

// hello opens (or revives, or adopts into) a workspace for a connection.
func (d *Daemon) hello(c *Conn, h *protocol.Hello) (*protocol.HelloAck, error) {
	if h.Protocol != protocol.Protocol {
		return nil, fmt.Errorf("protocol %d is not supported by daemon %s; run `ai-contextview serve --restart`", h.Protocol, d.Version)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastActive = time.Now()
	ack := &protocol.HelloAck{DaemonVersion: d.Version, URL: d.wsURL(h.WS)}

	w := d.workspaces[h.WS]
	if w != nil {
		// Same daemon, the bridge reconnected: revive.
		w.Live, w.conn, w.BridgePID, w.BridgePPID = true, c, h.BridgePID, h.BridgePPID
	} else {
		w = newWorkspace(h)
		w.conn = c
		if old := d.adoptable(h.Cwd); old != nil {
			w.adopt(old)
			delete(d.workspaces, old.ID)
			d.aliases[old.ID] = w.ID
			ack.AdoptedFrom = old.ID
			d.hub.Publish(old.ID, newEvent("redirect", map[string]string{"url": d.wsURL(w.ID)}))
		}
		d.workspaces[w.ID] = w
	}
	if cwd := d.takePendingCwd(h.ClaudePID, h.BridgePPID); cwd != "" {
		w.SessionCwd = cwd
	}
	d.publishAll(w)
	d.publishIndex()
	return ack, nil
}

// active reports whether a live workspace with an open, unfinished spec, or
// in ask mode, belongs to one of the given processes (the hook's ancestors).
// It matches the Claude Code process that started the bridge. A workspace
// released by turning ask mode off is not active, even with a spec open.
func (d *Daemon) active(pids []int, cwd string) *protocol.ActiveResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.sessionWorkspaces(pids) {
		d.setSessionCwd(w, cwd)
		// A document picked from a write is not a spec Claude opened: once
		// ask mode is off, it no longer blocks the built-in question tool.
		specOpen := w.Root != nil && w.Done == nil && !w.RootAuto && !w.Released
		if specOpen || w.AskMode {
			return &protocol.ActiveResult{Active: true, WS: w.ID, URL: d.wsURL(w.ID), AskMode: !specOpen, InChat: w.InChat}
		}
	}
	return &protocol.ActiveResult{}
}

// adoptable finds the most recent archived workspace with the same cwd,
// archived less than 2h ago.
func (d *Daemon) adoptable(cwd string) *Workspace {
	var best *Workspace
	for _, w := range d.workspaces {
		if w.Live || w.Cwd != cwd || time.Since(w.ArchivedAt) > adoptWindow {
			continue
		}
		if best == nil || w.ArchivedAt.After(best.ArchivedAt) {
			best = w
		}
	}
	return best
}

// archive marks a workspace as ended.
func (d *Daemon) archive(id string, c *Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil || w.conn != c {
		return
	}
	w.Live, w.conn, w.ArchivedAt = false, nil, time.Now()
	if w.Pending != nil {
		d.closePending(w, statusGone, nil, nil)
	}
	d.hub.Publish(w.ID, d.stateEvent(w), d.askEvent(w), d.historyEvent(w))
	d.pruneArchived()
	d.publishIndex()
	d.updateWatches()
}

func (d *Daemon) pruneArchived() {
	var archived []*Workspace
	for _, w := range d.workspaces {
		if !w.Live {
			archived = append(archived, w)
		}
	}
	if len(archived) <= maxArchived {
		return
	}
	sort.Slice(archived, func(i, j int) bool { return archived[i].ArchivedAt.Before(archived[j].ArchivedAt) })
	for _, w := range archived[:len(archived)-maxArchived] {
		delete(d.workspaces, w.ID)
	}
}

// Close removes an archived workspace (index page "Close").
func (d *Daemon) Close(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return errors.New("unknown workspace")
	}
	if w.Live {
		return errors.New("workspace is live")
	}
	delete(d.workspaces, id)
	d.publishIndex()
	d.updateWatches()
	return nil
}

// specShow sets the shown root.
func (d *Daemon) specShow(id string, req *protocol.SpecShow) (*protocol.SpecShowResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return nil, errors.New("unknown workspace")
	}
	prev := rootFile(w.Root)
	res, err := d.specShowLocked(w, req)
	if err == nil {
		// Showing another spec starts a new spec session: an earlier off no
		// longer applies. Showing the released spec again (spec-elaborate
		// does after every edit) keeps it released.
		if w.Released && rootFile(w.Root) != prev {
			w.Released = false
		}
		// Claude's own spec_show wins over the working document the daemon
		// picks from writes; a replay of a picked one does not.
		// A provisional document is shown like a picked one: a later write
		// can replace it, and it clears when ask mode ends.
		auto := req.Auto || req.Provisional
		w.work.explicit = !auto
		w.RootAuto = auto
		if auto {
			w.work.doc = req.RootAbs
		}
	}
	return res, err
}

// rootFile is the primary file of a root, or "" for none.
func rootFile(r *specsrc.Root) string {
	if r == nil {
		return ""
	}
	return filepath.Join(r.Dir, r.Primary)
}

// specShowLocked shows a root in w; d.mu is held.
func (d *Daemon) specShowLocked(w *Workspace, req *protocol.SpecShow) (*protocol.SpecShowResult, error) {
	root, err := specsrc.LoadRoot(req.RootAbs, req.Primary)
	if err != nil {
		return nil, err
	}
	sameDir := w.Root != nil && w.Root.Dir == root.Dir
	if !sameDir {
		w.files = map[string]*fileState{}
		w.extraDocs = map[string]bool{}
	}
	w.Root = root
	w.work.provisional = req.Provisional
	for _, a := range root.Artifacts {
		if w.files[a.Path] == nil {
			w.loadFile(a.Path, sameDir && root.IsDir)
		} else if w.fileChanged(a.Path) {
			w.loadFile(a.Path, false)
		}
	}
	w.hint(root.Primary, req.ChangedSections)
	for _, c := range req.Changed {
		rel := filepath.ToSlash(filepath.Clean(c.Path))
		if w.files[rel] != nil && len(c.Sections) == 0 {
			// A changed file without sections: mark it all.
			now := time.Now()
			for id := range w.files[rel].hashes {
				w.files[rel].changed[id] = now
			}
		}
		w.hint(rel, c.Sections)
	}
	if _, ok := w.docAbs(w.CurrentDoc); !ok || !sameDir {
		w.CurrentDoc = root.Primary
	}
	d.lastActive = time.Now()
	d.hub.Publish(w.ID, d.stateEvent(w), d.artifactsEvent(w), d.docEvent(w, w.CurrentDoc))
	if w.CurrentDoc != root.Primary {
		d.hub.Publish(w.ID, d.docEvent(w, root.Primary))
	}
	d.publishIndex()
	d.updateWatches()
	return &protocol.SpecShowResult{Primary: root.Primary, Sections: root.Artifact(root.Primary).Sections, Artifacts: root.Artifacts}, nil
}

// matchRef is matchDoc for a resolved ref; a line range always matches.
func (d *Daemon) matchRef(r protocol.ResolvedDocRef) protocol.Match {
	if r.StartLine > 0 {
		return protocol.MatchExact
	}
	return d.matchDoc(r.Path, r.Quote, r.Section)
}

func (d *Daemon) matchDoc(abs, quote, section string) protocol.Match {
	src, err := specsrc.ReadPrepared(abs)
	if err != nil {
		return protocol.MatchNone
	}
	return specsrc.Match(src, quote, section).Quality
}

// focus highlights refs without a question.
func (d *Daemon) focus(id string, req *protocol.Focus) (*protocol.FocusResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return nil, errors.New("unknown workspace")
	}
	res := &protocol.FocusResult{Docs: []protocol.DocMatch{}}
	w.focus = nil
	touched := map[string]bool{}
	for i, doc := range req.Docs {
		rel := w.docKey(doc.Path)
		w.focus = append(w.focus, docMark{Rel: rel, Ref: "f-" + itoa(i), Quote: doc.Quote, Section: doc.Section,
			StartLine: doc.StartLine, EndLine: doc.EndLine})
		res.Docs = append(res.Docs, protocol.DocMatch{Path: rel, Match: d.matchRef(doc)})
		touched[rel] = true
	}
	for _, c := range req.Code {
		w.addCode(c)
	}
	focus := map[string]any{}
	if len(req.Docs) > 0 {
		w.CurrentDoc = w.docKey(req.Docs[0].Path)
		focus["doc"] = map[string]string{"path": w.CurrentDoc, "ref": "f-0"}
	}
	if len(req.Code) > 0 {
		c := req.Code[0]
		focus["code"] = map[string]any{"path": c.Path, "start": c.StartLine, "end": c.EndLine}
	}
	touched[w.CurrentDoc] = true
	for rel := range touched {
		d.hub.Publish(w.ID, d.docEvent(w, rel))
	}
	d.hub.Publish(w.ID, d.codeEvent(w), newEvent("focus", focus))
	d.updateWatches()
	return res, nil
}

// ask shows a question set, replacing a pending one.
func (d *Daemon) ask(id string, req *protocol.Ask) (*protocol.AskShown, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return nil, errors.New("unknown workspace")
	}
	if w.InChat {
		// Nobody is at the viewer: Claude asks this set in the chat. Close a
		// set still pending and list this one in the history.
		if w.Pending != nil && w.Pending.Ask.QSID != req.QSID {
			old := w.Pending.Ask.QSID
			d.closePending(w, statusAskInChat, nil, nil)
			w.send(protocol.TypeAskResult, "", &protocol.AskResult{QSID: old, Status: protocol.StatusAskInChat})
		}
		w.History = append(w.History, &HistoryEntry{QS: &questionSet{Ask: *req, Shown: time.Now()}, Status: statusAskInChat, At: time.Now()})
		d.hub.Publish(w.ID, d.historyEvent(w))
		return &protocol.AskShown{QSID: req.QSID, Matches: []protocol.QuestionMatch{}, InChat: true}, nil
	}
	d.switchToAsked(w, req)
	if w.Pending != nil && w.Pending.Ask.QSID != req.QSID {
		old := w.Pending.Ask.QSID
		d.closePending(w, statusReplace, nil, nil)
		w.send(protocol.TypeAskResult, "", &protocol.AskResult{QSID: old, Status: protocol.StatusDismissed})
	}
	w.Pending = &questionSet{Ask: *req, Shown: time.Now()}
	w.Done = nil
	w.focus = nil
	shown := &protocol.AskShown{QSID: req.QSID, Matches: []protocol.QuestionMatch{}}
	touched := map[string]bool{}
	for qi, q := range req.Questions {
		for j, doc := range q.Docs {
			rel := w.docKey(doc.Path)
			touched[rel] = true
			if qi == 0 && j == 0 {
				w.CurrentDoc = rel
			}
			shown.Matches = append(shown.Matches, protocol.QuestionMatch{ID: q.ID, Doc: j, Match: d.matchRef(doc)})
		}
	}
	// Code tabs are this set's files, in question order, so Q1's come first.
	// Files from earlier sets stay reachable from history chips.
	w.Code = nil
	for _, q := range req.Questions {
		for _, c := range q.Code {
			w.addCode(c)
		}
	}
	touched[w.CurrentDoc] = true
	for rel := range touched {
		d.hub.Publish(w.ID, d.docEvent(w, rel))
	}
	d.hub.Publish(w.ID, d.codeEvent(w), d.askEvent(w), d.stateEvent(w))
	d.publishIndex()
	d.updateWatches()
	d.lastActive = time.Now()
	shown.Viewers, shown.Hidden = d.hub.Counts(w.ID)
	return shown, nil
}

// cancel withdraws a pending question set (ask_cancel).
func (d *Daemon) cancel(id string, req *protocol.AskCancel) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return errors.New("unknown workspace")
	}
	if w.Pending == nil || w.Pending.Ask.QSID != req.QSID {
		return &specsrc.Error{Code: protocol.CodeUnknownQS, Message: "no pending question set " + req.QSID}
	}
	d.closePending(w, statusCancel, nil, nil)
	d.publishIndex()
	return nil
}

// done marks the spec as finished (spec_done). A pending question set is
// withdrawn: Claude said no more answers are needed. Highlights and code
// tabs are cleared.
// askHere moves the workspace's questions to the chat or back to the
// viewer (ask_here). Moving them to the chat closes a pending set with
// ask_in_chat, which ends a waiting ask or ask_wait.
func (d *Daemon) askHere(id string, req *protocol.AskHere) (*protocol.AskHereResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return nil, errors.New("unknown workspace")
	}
	res := &protocol.AskHereResult{InChat: req.On}
	if req.On && req.Auto && !w.SeenAt.IsZero() && w.Pending != nil && !w.SeenAt.Before(w.Pending.Shown) {
		// Someone used the page after the bridge decided nobody had: keep
		// the questions in the viewer.
		res.InChat = false
		return res, nil
	}
	w.InChat = req.On
	if req.On && w.Pending != nil {
		res.Closed = w.Pending.Ask.QSID
		d.closePending(w, statusAskInChat, nil, nil)
		w.send(protocol.TypeAskResult, "", &protocol.AskResult{QSID: res.Closed, Status: protocol.StatusAskInChat})
	}
	d.hub.Publish(w.ID, d.stateEvent(w))
	d.publishIndex()
	return res, nil
}

// askMode turns ask mode on or off for a workspace.
func (d *Daemon) askMode(id string, req *protocol.AskMode) (*protocol.AskModeResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return nil, errors.New("unknown workspace")
	}
	w.AskMode = req.On
	w.Released = !req.On
	w.work.spec = req.On && req.Spec
	w.work.paths = nil
	if w.work.spec {
		w.work.paths = req.Paths
	}
	if !req.On {
		w.work = workDoc{explicit: w.work.explicit}
		if w.RootAuto {
			// A document picked from a write only served ask mode: clear it,
			// so the page goes back to its state without a document.
			d.clearRoot(w)
		}
	}
	res := &protocol.AskModeResult{}
	if p := d.showExisting(w); p != "" {
		res.Shown = p
	}
	d.hub.Publish(w.ID, d.docEvent(w, w.CurrentDoc))
	return res, nil
}

// clearRoot removes the shown root and its documents; d.mu is held.
func (d *Daemon) clearRoot(w *Workspace) {
	w.Root = nil
	w.RootAuto = false
	w.files = map[string]*fileState{}
	w.extraDocs = map[string]bool{}
	w.CurrentDoc = ""
	w.focus = nil
	d.hub.Publish(w.ID, d.stateEvent(w), d.artifactsEvent(w))
	d.publishIndex()
	d.updateWatches()
}

func (d *Daemon) done(id string, req *protocol.Done) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil {
		return errors.New("unknown workspace")
	}
	if w.Pending != nil {
		qs := w.Pending.Ask.QSID
		d.closePending(w, statusCancel, nil, nil)
		w.send(protocol.TypeAskResult, "", &protocol.AskResult{QSID: qs, Status: protocol.StatusDismissed})
	}
	// The session is over: clear every highlight and close the code tabs.
	touched := map[string]bool{w.CurrentDoc: true}
	for _, m := range w.focus {
		touched[m.Rel] = true
	}
	w.focus = nil
	w.Code = nil
	w.Done = &DoneState{Summary: req.Summary, At: time.Now()}
	for rel := range touched {
		if rel != "" {
			d.hub.Publish(w.ID, d.docEvent(w, rel))
		}
	}
	d.hub.Publish(w.ID, d.codeEvent(w), d.stateEvent(w), d.askEvent(w))
	d.publishIndex()
	return nil
}

// ErrNotPending is returned when answering a question set that is no longer
// pending.
var ErrNotPending = errors.New("question set is no longer pending")

// ErrGone is returned when the workspace has no bridge connection.
var ErrGone = errors.New("Claude is disconnected")

// Answer closes the pending question set with the user's action and sends
// the result to the bridge. The first answer wins.
func (d *Daemon) Answer(id, qs, status string, answers []protocol.Answer, keepOpen, deferred []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	w, _ := d.lookup(id)
	if w == nil || w.Pending == nil || w.Pending.Ask.QSID != qs {
		return ErrNotPending
	}
	if !w.Live || w.conn == nil {
		return ErrGone
	}
	res := &protocol.AskResult{QSID: qs, Status: status, Answers: answers, Deferred: deferred, KeepOpen: keepOpen}
	if err := res.Validate(); err != nil {
		return err
	}
	w.send(protocol.TypeAskResult, "", res)
	// The user replied: this is the new base of "since your last answers".
	w.snapshotAnswers()
	byID := map[string]protocol.Answer{}
	for _, a := range answers {
		byID[a.ID] = a
	}
	d.closePending(w, status, byID, keepOpen)
	if n := len(w.History); n > 0 {
		w.History[n-1].Deferred = deferred
		d.hub.Publish(w.ID, d.historyEvent(w))
	}
	d.publishIndex()
	return nil
}

// closePending moves the pending set to history and updates pages.
func (d *Daemon) closePending(w *Workspace, status string, answers map[string]protocol.Answer, keepOpen []string) {
	qs := w.Pending
	touched := map[string]bool{w.CurrentDoc: true}
	for _, m := range w.pendingMarks() {
		touched[m.Rel] = true
	}
	w.History = append(w.History, &HistoryEntry{QS: qs, Status: status, Answers: answers, KeepOpen: keepOpen, At: time.Now()})
	w.Pending = nil
	for rel := range touched {
		if rel != "" {
			d.hub.Publish(w.ID, d.docEvent(w, rel))
		}
	}
	d.hub.Publish(w.ID, newEvent("ask_closed", map[string]string{"qs_id": qs.Ask.QSID, "status": status}),
		d.askEvent(w), d.historyEvent(w), d.stateEvent(w))
}

// --------------------------------------------------------------- refresh

// refresh re-reads changed files of a workspace and pushes updates.
func (d *Daemon) refresh(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workspaces[id]
	if w == nil || w.Root == nil {
		return
	}
	var docs []string
	listChanged := false
	if w.Root.IsDir {
		if files, err := specsrc.ListMarkdown(w.Root.Dir); err == nil && !sameList(files, w.Root.Artifacts) {
			if root, err := specsrc.LoadRoot(w.Root.Abs, w.Root.Primary); err == nil {
				w.Root = root
				listChanged = true
			} else if root, err := specsrc.LoadRoot(w.Root.Abs, ""); err == nil {
				w.Root = root
				listChanged = true
			}
		}
	}
	for _, a := range w.Root.Artifacts {
		if w.files[a.Path] == nil {
			w.loadFile(a.Path, true)
			docs = append(docs, a.Path)
		} else if w.fileChanged(a.Path) {
			w.loadFile(a.Path, false)
			docs = append(docs, a.Path)
		}
	}
	for rel := range w.extraDocs {
		if w.fileChanged(rel) {
			w.loadFile(rel, false)
			docs = append(docs, rel)
		}
	}
	if len(docs) > 0 {
		// Sections may have been renamed: refresh the artifact list too.
		if root, err := specsrc.LoadRoot(w.Root.Abs, primaryArg(w.Root)); err == nil {
			w.Root = root
		}
		listChanged = true
	}
	if _, ok := w.docAbs(w.CurrentDoc); !ok {
		w.CurrentDoc = w.Root.Primary
		docs = append(docs, w.CurrentDoc)
	}
	codeChanged := false
	for _, t := range w.Code {
		st, err := os.Stat(t.Path)
		if err != nil || (st.ModTime().Equal(t.mtime) && st.Size() == t.size) {
			continue
		}
		t.mtime, t.size = st.ModTime(), st.Size()
		if src, err := os.ReadFile(t.Path); err == nil {
			n := render.LineCount(src)
			if t.Lines != 0 && n != t.Lines {
				t.Stale = true
			}
			t.Lines = n
		}
		codeChanged = true
	}
	if listChanged {
		d.hub.Publish(w.ID, d.artifactsEvent(w), d.stateEvent(w))
	}
	for _, rel := range docs {
		d.hub.Publish(w.ID, d.docEvent(w, rel))
	}
	if codeChanged {
		d.hub.Publish(w.ID, d.codeEvent(w))
	}
	if listChanged {
		d.updateWatches()
	}
}

func primaryArg(r *specsrc.Root) string {
	if r.IsDir {
		return r.Primary
	}
	return ""
}

func sameList(files []string, arts []protocol.Artifact) bool {
	if len(files) != len(arts) {
		return false
	}
	set := map[string]bool{}
	for _, a := range arts {
		set[a.Path] = true
	}
	for _, f := range files {
		if !set[f] {
			return false
		}
	}
	return true
}

// --------------------------------------------------------------- watching

// updateWatches makes the fsnotify watch set match the live workspaces.
func (d *Daemon) updateWatches() {
	if d.watcher == nil {
		return
	}
	want := map[string]bool{}
	for _, w := range d.workspaces {
		if !w.Live {
			continue
		}
		for _, dir := range w.watchDirs() {
			want[dir] = true
		}
	}
	for dir := range d.watched {
		if !want[dir] {
			_ = d.watcher.Remove(dir)
			delete(d.watched, dir)
		}
	}
	for dir := range want {
		if !d.watched[dir] {
			if err := d.watcher.Add(dir); err == nil {
				d.watched[dir] = true
			}
		}
	}
}

// watchLoop turns fsnotify events into debounced workspace refreshes. Watching
// directories (not files) survives atomic saves.
func (d *Daemon) watchLoop() {
	for {
		select {
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			dir := filepath.Dir(ev.Name)
			d.mu.Lock()
			for _, w := range d.workspaces {
				if !w.Live {
					continue
				}
				for _, wd := range w.watchDirs() {
					if wd == dir || wd == ev.Name {
						d.schedule(w.ID)
						break
					}
				}
			}
			d.mu.Unlock()
		case _, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
		case <-d.shutdown:
			return
		}
	}
}

const debounce = 150 * time.Millisecond

// schedule debounces a refresh. Callers hold d.mu.
func (d *Daemon) schedule(id string) {
	if t := d.timers[id]; t != nil {
		t.Reset(debounce)
		return
	}
	d.timers[id] = time.AfterFunc(debounce, func() {
		d.mu.Lock()
		delete(d.timers, id)
		d.mu.Unlock()
		d.refresh(id)
	})
}

// pollLoop is the safety net for missed fsnotify events: it checks mtimes
// every 2s.
func (d *Daemon) pollLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.mu.Lock()
			var ids []string
			for _, w := range d.workspaces {
				if w.Live {
					ids = append(ids, w.ID)
				}
			}
			d.mu.Unlock()
			for _, id := range ids {
				d.refresh(id)
			}
		case <-d.shutdown:
			return
		}
	}
}

// Idle reports how long the daemon had no bridges and no pages.
func (d *Daemon) Idle() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.workspaces {
		if w.Live {
			return 0
		}
	}
	if d.hub.Total() > 0 {
		return 0
	}
	return time.Since(d.lastActive)
}

// touch records activity (page views).
func (d *Daemon) touch() {
	d.mu.Lock()
	d.lastActive = time.Now()
	d.mu.Unlock()
}

// viewers answers a bridge's viewers request: the open and hidden pages of
// the workspace, and whether someone used one since the pending question
// set was shown.
func (d *Daemon) viewers(id string) *protocol.ViewersResult {
	res := &protocol.ViewersResult{Presence: true}
	res.Viewers, res.Hidden = d.hub.Counts(id)
	d.mu.Lock()
	defer d.mu.Unlock()
	if w := d.workspaces[id]; w != nil && w.Pending != nil {
		res.Seen = !w.SeenAt.Before(w.Pending.Shown)
	}
	return res
}
