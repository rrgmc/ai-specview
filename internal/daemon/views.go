package daemon

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/render"
	"github.com/rrgmc/ai-contextview/internal/specsrc"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"hhmm": func(t time.Time) string { return t.Format("15:04") },
	"ago": func(t time.Time) string {
		d := time.Since(t).Round(time.Minute)
		if d < time.Minute {
			return "just now"
		}
		return d.String() + " ago"
	},
	"inc":  func(i int) int { return i + 1 },
	"join": strings.Join,
}).ParseFS(templateFS, "templates/*.html"))

func execTemplate(name string, data any) string {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return template.HTMLEscapeString("template error: " + err.Error())
	}
	return buf.String()
}

// ------------------------------------------------------------ view models

type chipView struct {
	Kind  string // doc | code | nodoc
	Label string
	Title string
	Badge string
	Bad   bool
	Doc   string // doc key
	Ref   string // data-ref of the mark
	Path  string // code path
	Start int
	End   int
}

type questionView struct {
	Index   int
	ID      string
	Text    string
	Options []protocol.Option
	Multi   bool
	Chips   []chipView
	Answer  *protocol.Answer
	Keep    bool
	Later   bool
	// OwnLater is set when one of the question's own options already
	// means "decide later"; the panel then leaves out its own choice.
	OwnLater bool
}

type qsView struct {
	WS        string
	ID        string
	Title     string
	Count     int
	Folder    bool
	Questions []questionView
}

type historyView struct {
	Title     string
	Status    string
	At        time.Time
	Questions []questionView
}

func (d *Daemon) chips(w *Workspace, q protocol.AskQuestion) []chipView {
	var out []chipView
	for j, doc := range q.Docs {
		rel := w.docKey(doc.Path)
		label := "§ " + doc.Section
		name := w.docName(rel)
		title := name + ": " + doc.Quote
		switch {
		case doc.StartLine > 0:
			label = filepath.Base(rel) + ":" + lineLabel(doc.StartLine, doc.EndLine)
			title = name + ":" + lineLabel(doc.StartLine, doc.EndLine)
		case doc.Section == "":
			label = "“" + truncate(doc.Quote, 32) + "”"
		}
		if doc.StartLine == 0 && (w.Root == nil || rel != w.Root.Primary) {
			label = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)) + " " + label
		}
		c := chipView{Kind: "doc", Label: label, Title: title, Doc: rel, Ref: markRef(q.ID, j)}
		switch d.matchRef(doc) {
		case protocol.MatchFuzzy:
			c.Badge = "≈ fuzzy"
		case protocol.MatchHeading:
			c.Badge = "§ section only"
		case protocol.MatchNone:
			c.Badge, c.Bad = "✗ not found", true
		}
		out = append(out, c)
	}
	for _, r := range q.Code {
		label := fmt.Sprintf("%s:%d-%d", filepath.Base(r.Path), r.StartLine, r.EndLine)
		if r.StartLine == r.EndLine {
			label = fmt.Sprintf("%s:%d", filepath.Base(r.Path), r.StartLine)
		}
		title := r.Path
		if r.Symbol != "" {
			title += " · " + r.Symbol
		}
		out = append(out, chipView{Kind: "code", Label: label, Title: title, Path: r.Path, Start: r.StartLine, End: r.EndLine})
	}
	return out
}

// hasLaterOption reports whether one of the options already offers to
// decide later, such as "Decide later", "Decide this later" or "Defer".
// Keep it in step with hasLaterOption in plugin/hooks/register.tsx.
func hasLaterOption(opts []protocol.Option) bool {
	for _, o := range opts {
		if isLaterLabel(o.Label) {
			return true
		}
	}
	return false
}

func isLaterLabel(label string) bool {
	words := strings.FieldsFunc(strings.ToLower(label), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	l := strings.Join(words, " ")
	for _, p := range []string{"decide later", "decide it later", "decide this later", "decide that later"} {
		if l == p || strings.HasPrefix(l, p+" ") {
			return true
		}
	}
	switch l {
	case "later", "defer", "defer it", "defer this", "defer for now", "defer decision", "defer the decision":
		return true
	}
	return false
}

func lineLabel(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "-" + strconv.Itoa(end)
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func (d *Daemon) questions(w *Workspace, qs *questionSet, answers map[string]protocol.Answer, keep []string) []questionView {
	keepSet := map[string]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}
	laterSet := map[string]bool{}
	for _, a := range answers {
		if a.Deferred {
			laterSet[a.ID] = true
		}
	}
	var out []questionView
	for i, q := range qs.Ask.Questions {
		v := questionView{Index: i, ID: q.ID, Text: q.Text, Options: q.Options, Multi: q.MultiSelect,
			Chips: d.chips(w, q), Keep: keepSet[q.ID], Later: laterSet[q.ID], OwnLater: hasLaterOption(q.Options)}
		if a, ok := answers[q.ID]; ok {
			a := a
			v.Answer = &a
		}
		out = append(out, v)
	}
	return out
}

// ------------------------------------------------------------------ events

func (d *Daemon) stateEvent(w *Workspace) Event {
	root := ""
	if w.Root != nil {
		root = filepath.Base(w.Root.Abs)
	}
	return newEvent("state", map[string]any{
		"live": w.Live, "project": w.Project, "cwd": w.Cwd, "claude_pid": w.ClaudePID,
		"started": w.Started.Format("15:04"), "root": root, "is_dir": w.Root != nil && w.Root.IsDir,
		"open_questions": w.openQuestions(), "pending": w.Pending != nil, "done": w.Done != nil,
		"has_spec": w.Root != nil, "provisional": w.Root != nil && w.work.provisional,
		"in_chat": w.InChat,
	})
}

type artifactGroup struct {
	Kind  string
	Label string
	Items []artifactItem
}

type artifactItem struct {
	Path    string
	Name    string
	Primary bool
	Changed bool
}

var kindLabels = map[protocol.ArtifactKind]string{
	protocol.KindSpec: "Spec", protocol.KindADR: "Decisions", protocol.KindResearch: "Research",
	protocol.KindOpenQuestion: "Open questions", protocol.KindOther: "Other",
}

func (d *Daemon) artifactsEvent(w *Workspace) Event {
	if w.Root == nil || !w.Root.IsDir {
		return newEvent("artifacts", map[string]any{"html": ""})
	}
	order := []protocol.ArtifactKind{protocol.KindSpec, protocol.KindADR, protocol.KindResearch, protocol.KindOpenQuestion, protocol.KindOther}
	var groups []artifactGroup
	for _, k := range order {
		g := artifactGroup{Kind: string(k), Label: kindLabels[k]}
		for _, a := range w.Root.Artifacts {
			if a.Kind == k {
				g.Items = append(g.Items, artifactItem{Path: a.Path, Name: a.Path, Primary: a.Path == w.Root.Primary, Changed: len(w.changedIDs(a.Path)) > 0})
			}
		}
		if len(g.Items) > 0 {
			sort.SliceStable(g.Items, func(i, j int) bool { return g.Items[i].Primary && !g.Items[j].Primary })
			groups = append(groups, g)
		}
	}
	return newEvent("artifacts", map[string]any{"html": execTemplate("artifacts", groups)})
}

// docPayload renders one doc with its marks and changed sections.
// lineCount is the number of lines of a file as an editor counts them: a
// last line without a newline counts, an empty file has none.
func lineCount(b []byte) int {
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func (d *Daemon) docPayload(w *Workspace, rel string) map[string]any {
	out := map[string]any{"path": rel}
	abs, ok := w.docAbs(rel)
	if !ok && w.AskMode && w.Root == nil {
		out["html"] = `<p class="empty">Ask mode is on: Claude asks every question of this session here. No spec is open.</p>`
		return out
	}
	if !ok {
		out["html"] = `<p class="empty">No spec shown yet. The skill calls <code>spec_show</code> when it creates or opens a spec.</p>`
		return out
	}
	out["abs"] = abs
	out["name"] = w.docName(rel)
	if !specsrc.IsMarkdown(abs) {
		// A text document that is not markdown: its source, with the line
		// range of the current question highlighted.
		src, err := os.ReadFile(abs)
		if err != nil {
			out["html"] = `<p class="empty">` + template.HTMLEscapeString(err.Error()) + `</p>`
			return out
		}
		out["lines"] = lineCount(src)
		start, end := w.lineMark(rel)
		html, err := render.Code(abs, src, start, end)
		if err != nil {
			html = `<p class="empty">` + template.HTMLEscapeString(err.Error()) + `</p>`
		}
		out["html"] = `<div class="doc-src">` + html + `</div>`
		out["source"] = true
		return out
	}
	src, err := specsrc.ReadPrepared(abs)
	if err != nil {
		out["html"] = `<p class="empty">` + template.HTMLEscapeString(err.Error()) + `</p>`
		return out
	}
	// The frontmatter is blanked, not removed, so the count matches the file.
	out["lines"] = lineCount(src)
	changed := w.changedIDs(rel)
	html, _, err := render.Markdown(src, w.marksFor(rel, src), changed)
	if err != nil {
		html = `<p class="empty">` + template.HTMLEscapeString(err.Error()) + `</p>`
	}
	kind := protocol.KindOther
	if w.Root != nil {
		if a := w.Root.Artifact(rel); a != nil {
			kind = a.Kind
		}
	}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out["html"] = html
	base := w.defaultBase()
	if w.files[rel] != nil {
		out["changes"] = render.Hunks(render.DiffBlocks(w.baseFor(rel, base), src))
	}
	out["changes_base"] = base
	out["kind"] = kind
	out["primary"] = w.Root != nil && rel == w.Root.Primary
	out["changed"] = ids
	return out
}

func (d *Daemon) docEvent(w *Workspace, rel string) Event {
	return newEvent("doc", d.docPayload(w, rel))
}

func (d *Daemon) codePayload(w *Workspace, t *CodeTab, start, end int) map[string]any {
	rel := t.Path
	if r, err := filepath.Rel(w.projectDir(), t.Path); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	out := map[string]any{"path": t.Path, "rel": rel, "name": filepath.Base(t.Path), "start": start, "end": end, "stale": t.Stale}
	src, err := os.ReadFile(t.Path)
	if err != nil {
		out["html"] = `<p class="empty">` + template.HTMLEscapeString(err.Error()) + `</p>`
		return out
	}
	html, err := render.Code(t.Path, src, start, end)
	if err != nil {
		html = `<p class="empty">` + template.HTMLEscapeString(err.Error()) + `</p>`
	}
	out["html"] = html
	return out
}

func (d *Daemon) codeEvent(w *Workspace) Event {
	tabs := make([]map[string]any, 0, len(w.Code))
	for _, t := range w.Code {
		tabs = append(tabs, d.codePayload(w, t, t.Start, t.End))
	}
	return newEvent("code", map[string]any{"tabs": tabs})
}

func (d *Daemon) askEvent(w *Workspace) Event {
	if w.Pending == nil {
		summary := ""
		if n := len(w.History); n > 0 {
			h := w.History[n-1]
			summary = fmt.Sprintf("%s %d question%s · %s", statusVerb(h.Status), len(h.QS.Ask.Questions), plural(len(h.QS.Ask.Questions)), h.At.Format("15:04"))
		}
		out := map[string]any{"qs_id": "", "summary": summary, "project": w.Project}
		if w.Done != nil {
			html := ""
			if w.Done.Summary != "" {
				if h, _, err := render.Markdown([]byte(w.Done.Summary), nil, nil); err == nil {
					html = h
				}
			}
			out["done"] = true
			out["done_at"] = w.Done.At.Format("15:04")
			out["done_html"] = html
		}
		return newEvent("ask", out)
	}
	qs := w.Pending
	v := qsView{WS: w.ID, ID: qs.Ask.QSID, Title: qs.Ask.Title, Count: len(qs.Ask.Questions),
		Folder: w.Root != nil && w.Root.IsDir, Questions: d.questions(w, qs, nil, nil)}
	first := ""
	if len(qs.Ask.Questions) > 0 {
		first = qs.Ask.Questions[0].Text
	}
	// The document panel shows for the whole set when any question has a
	// document or a spec is shown, so it does not open and close between
	// the questions of one set.
	docPanel := w.Root != nil
	for _, q := range qs.Ask.Questions {
		docPanel = docPanel || len(q.Docs) > 0
	}
	if docPanel {
		// With the document panel shown, a question with no document says
		// so under its text, where the user reads it.
		for i, q := range qs.Ask.Questions {
			if len(q.Docs) == 0 {
				v.Questions[i].Chips = append([]chipView{{Kind: "nodoc", Label: "No document",
					Title: "This question is not about a passage of a document; the panel keeps the last document, without a highlight"}},
					v.Questions[i].Chips...)
			}
		}
	}
	return newEvent("ask", map[string]any{
		"qs_id": qs.Ask.QSID, "html": execTemplate("panel", v), "title": qs.Ask.Title,
		"first": first, "count": len(qs.Ask.Questions), "project": w.Project, "doc_panel": docPanel,
	})
}

func statusVerb(s string) string {
	switch s {
	case protocol.StatusAnswered:
		return "Answered"
	case protocol.StatusInChat:
		return "Moved to chat:"
	case protocol.StatusDismissed:
		return "Skipped"
	case statusReplace:
		return "Replaced"
	case statusCancel:
		return "Cancelled"
	case statusGone:
		return "Claude disconnected:"
	case statusAskInChat:
		return "Asked in chat:"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (d *Daemon) historyEvent(w *Workspace) Event {
	var items []historyView
	for i := len(w.History) - 1; i >= 0; i-- {
		h := w.History[i]
		items = append(items, historyView{Title: h.QS.Ask.Title, Status: statusVerb(h.Status), At: h.At,
			Questions: d.questions(w, h.QS, h.Answers, h.KeepOpen)})
	}
	return newEvent("history", map[string]any{"html": execTemplate("history", items), "count": len(items)})
}

// snapshot is the full state sent when a page connects.
func (d *Daemon) snapshot(w *Workspace) []Event {
	return []Event{d.stateEvent(w), d.artifactsEvent(w), d.docEvent(w, w.CurrentDoc), d.codeEvent(w), d.askEvent(w), d.historyEvent(w)}
}

func (d *Daemon) publishAll(w *Workspace) {
	d.hub.Publish(w.ID, d.snapshot(w)...)
}

func (d *Daemon) publishIndex() {
	d.hub.Publish(indexKey, Event{Name: "changed", Data: []byte("{}")})
}

// ------------------------------------------------------------- index page

type indexItem struct {
	ID       string
	Project  string
	Cwd      string
	Root     string
	Live     bool
	Pending  bool
	Done     bool
	Started  time.Time
	Archived time.Time
	Viewers  int
}

func (d *Daemon) indexItems() []indexItem {
	var out []indexItem
	for _, w := range d.workspaces {
		root := ""
		if w.Root != nil {
			root = filepath.Base(w.Root.Abs)
		}
		out = append(out, indexItem{ID: w.ID, Project: w.Project, Cwd: w.Cwd, Root: root, Live: w.Live,
			Pending: w.Pending != nil, Done: w.Done != nil, Started: w.Started, Archived: w.ArchivedAt, Viewers: d.hub.Viewers(w.ID)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		if out[i].Live {
			return out[i].Started.After(out[j].Started)
		}
		return out[i].Archived.After(out[j].Archived)
	})
	return out
}
