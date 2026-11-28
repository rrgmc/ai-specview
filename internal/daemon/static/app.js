// ai-contextview workspace page. Server-Sent Events carry state; answers go back as
// POSTs. No build step, no framework.
(function () {
  "use strict";

  const ws = document.body.dataset.ws;
  const base = "/w/" + encodeURIComponent(ws);
  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

  const store = {
    get(k, d) { try { const v = localStorage.getItem("ai-contextview." + k); return v === null ? d : v; } catch (e) { return d; } },
    set(k, v) { try { localStorage.setItem("ai-contextview." + k, v); } catch (e) { /* private mode */ } },
  };

  const docEl = $("#doc");
  const codeEl = $("#code");
  const layout = $("#layout");
  const panel = $("#panel");

  let currentDoc = null;     // doc key shown in this page
  let activeRef = null;      // {path, ref} of the highlighted passage, if any
  let docView = "spec";      // "spec" or "changes"
  let chgBase = "";          // "", "answers" or "start" ("" = the server's default)
  let chgMode = "rendered";  // "rendered" or "source"
  const changesEl = $("#changes");
  let currentAbs = "";
  let codeTabs = [];         // payloads from the server
  let activeCode = null;     // path
  let noCode = false;        // the shown question has no code ref
  let pendingQS = "";        // qs id of the shown question set
  let doneShown = false;     // the finished banner is on screen
  let qIndex = 0;
  let reviewing = false;     // the review replaces the question view
  let live = true;
  let baseTitle = document.title;
  let hasSpec = false;       // a spec is shown: the document panel always shows
  let sawState = false;

  // ------------------------------------------------------------ helpers

  function toast(msg) {
    const t = $("#toast");
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toast.timer);
    toast.timer = setTimeout(() => { t.hidden = true; }, 2600);
  }

  // "Open in editor" asks the daemon to start the editor: a link to the
  // editor's URL scheme does nothing in a browser that does not hand such
  // links to the system, such as the desktop app's browser pane.
  async function openInEditor(kind, path, line) {
    const body = new URLSearchParams({ kind, path, line: String(line || 0), editor: store.get("editor", "vscode") });
    try {
      const r = await fetch(base + "/open", {
        method: "POST", body, headers: { "Content-Type": "application/x-www-form-urlencoded" },
      });
      if (!r.ok) toast("Cannot open the editor: " + (await r.text()).trim());
    } catch (e) { toast("Cannot open the editor"); }
  }

  document.addEventListener("click", (e) => {
    const a = e.target.closest("a.open-editor");
    if (!a) return;
    e.preventDefault();
    openInEditor(a.dataset.kind, a.dataset.path, Number(a.dataset.line) || 0);
  });

  async function getJSON(url) {
    const r = await fetch(url, { headers: { Accept: "application/json" } });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  // ------------------------------------------------------------ doc pane

  function anchorHeading() {
    const top = docEl.scrollTop;
    let best = null;
    for (const h of $$("h1[id],h2[id],h3[id],h4[id],h5[id],h6[id]", docEl)) {
      if (h.offsetTop <= top + 4) best = h; else break;
    }
    return best ? { id: best.id, off: top - best.offsetTop } : { id: null, off: top };
  }

  function setDoc(p, keepScroll) {
    const anchor = keepScroll && p.path === currentDoc ? anchorHeading() : null;
    currentDoc = p.path;
    currentAbs = p.abs || "";
    docEl.innerHTML = p.html || "";
    $("#doc-title").textContent = p.name || p.path || "";
    $("#docname").textContent = p.name || p.path || "";
    const link = $("#doc-editor");
    link.hidden = !currentAbs;
    link.dataset.path = currentDoc || "";
    if (anchor) {
      const h = anchor.id && document.getElementById(anchor.id);
      docEl.scrollTop = h ? h.offsetTop + anchor.off : anchor.off;
    }
    $$(".artifact").forEach((a) => a.classList.toggle("current", a.dataset.doc === currentDoc));
    $("#chg-count").textContent = p.changes ? " (" + p.changes + ")" : "";
    docLines = p.lines || 0;
    if (docView === "changes") loadChanges();
    updateJump();
  }

  // The line count of the document shows only while the header has room for
  // it: it is the first thing to go when the pane is narrow.
  let docLines = 0;
  function fitDocLines() {
    const el = $("#doc-lines");
    el.textContent = docLines ? docLines.toLocaleString() + (docLines === 1 ? " line" : " lines") : "";
    el.hidden = !docLines;
    const head = el.parentElement;
    if (!el.hidden && head.scrollWidth > head.clientWidth) el.hidden = true;
  }
  {
    // Refit when the header changes size or its other content changes.
    const el = $("#doc-lines");
    const head = el.parentElement;
    new ResizeObserver(fitDocLines).observe(head);
    new MutationObserver((recs) => {
      if (recs.some((r) => r.target !== el && !el.contains(r.target))) fitDocLines();
    }).observe(head, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ["hidden"] });
  }

  // ------------------------------------------------------------ changes view

  async function loadChanges() {
    if (!currentDoc) return;
    // Requests can finish out of order (a doc switch, a refresh, a new base
    // or mode). Only the newest one may fill the pane, so the diff always
    // belongs to the doc the Spec tab shows.
    const seq = loadChanges.seq = (loadChanges.seq || 0) + 1;
    const path = currentDoc;
    const mode = chgMode;
    if (path !== loadChanges.shown) {
      changesEl.innerHTML = '<p class="empty">Loading the changes…</p>';
      loadChanges.shown = path;
    }
    const q = "?path=" + encodeURIComponent(path) + (chgBase ? "&base=" + chgBase : "") + "&mode=" + mode;
    try {
      const c = await getJSON(base + "/changes" + q);
      if (seq !== loadChanges.seq) return;
      // The whole document is shown: on first open (or a new base or
      // mode) scroll to the first change; on a refresh keep the position.
      const key = path + "|" + c.base + "|" + mode;
      const keep = changesEl.scrollTop;
      changesEl.innerHTML = c.html;
      const first = $(".chg-add, .chg-del, .chg-row-add, .chg-row-del, .sd-add, .sd-del", changesEl);
      if (key !== loadChanges.last && first) scrollIntoPane(changesEl, first);
      else changesEl.scrollTop = keep;
      loadChanges.last = key;
      const sel = $("#chg-base");
      sel.querySelector('option[value="answers"]').disabled = !c.has_answers;
      sel.value = c.base;
      $("#chg-since").textContent = (c.count ? c.count + " change" + (c.count === 1 ? "" : "s") : "No changes") + " since " + c.since;
    } catch (e) {
      if (seq === loadChanges.seq) changesEl.innerHTML = '<p class="empty">Cannot load the changes.</p>';
    }
  }

  function setView(v) {
    docView = v;
    $$(".dtab").forEach((t) => t.classList.toggle("active", t.dataset.view === v));
    docEl.hidden = v !== "spec";
    changesEl.hidden = v !== "changes";
    $("#chg-bar").hidden = v !== "changes";
    if (v === "changes") loadChanges();
    updateJump();
  }

  async function loadDoc(path) {
    if (path === currentDoc) return;
    // Two quick clicks: only the last doc asked for is shown.
    const seq = loadDoc.seq = (loadDoc.seq || 0) + 1;
    try {
      const p = await getJSON(base + "/doc?path=" + encodeURIComponent(path));
      if (seq !== loadDoc.seq) return;
      setDoc(p, false);
      docEl.scrollTop = 0;
    } catch (e) { toast("Cannot open " + path); }
  }

  async function focusDoc(path, ref) {
    if (path && path !== currentDoc) await loadDoc(path);
    if (ref && docView !== "spec") setView("spec");
    $$("mark.q.active, .q-section.active", docEl).forEach((m) => m.classList.remove("active"));
    if (!ref) { updateJump(); return; }
    activeRef = { path: path || currentDoc, ref };
    const marks = $$('[data-ref="' + CSS.escape(ref) + '"]', docEl);
    marks.forEach((m) => m.classList.add("active"));
    // A source document marks its lines, not a data-ref.
    const at = marks[0] || $(".line.hl", docEl);
    if (at) scrollIntoPane(docEl, at);
    updateJump();
  }

  // The document panel shows for a whole question set, or always with a spec.
  function setDocPanel(on) {
    layout.classList.toggle("no-doc", !on);
    if (!on) setZoom(false);
    $("#docname").hidden = !on;
  }

  // Tabs for the documents of the current question, when it has more than one.
  function renderQDocTabs(sec) {
    const box = $("#qdoc-tabs");
    box.innerHTML = "";
    const seen = new Map();
    if (sec) $$(".chip-doc", sec).forEach((c) => { if (!seen.has(c.dataset.doc)) seen.set(c.dataset.doc, c); });
    box.hidden = seen.size < 2;
    $("#doc-title").hidden = seen.size >= 2;
    if (seen.size < 2) return;
    for (const [path, chip] of seen) {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "tab" + (path === currentDoc ? " active" : "");
      b.title = chip.title;
      b.textContent = path.split("/").pop();
      b.addEventListener("click", () => focusDoc(path, chip.dataset.ref).then(() => renderQDocTabs(sec)));
      box.appendChild(b);
    }
  }

  // "Show current passage" is shown while a passage is highlighted for the
  // current question or a focus call. In the shown doc it needs the mark to
  // still exist; another doc is loaded on click.
  function updateJump() {
    const link = $("#doc-jump");
    let show = !!activeRef && docView === "spec";
    if (show && activeRef.path === currentDoc) {
      show = $$('[data-ref="' + CSS.escape(activeRef.ref) + '"]', docEl).length > 0;
    }
    link.hidden = !show;
    fitDocLines();
  }

  // ------------------------------------------------------------ code pane

  // The code pane shows only while it shows a file. With the questions above
  // the code, the questions then take the whole column; the CSS keeps it in
  // the layouts where hiding it would move a pane sideways.
  function setCodeShown(on) {
    layout.classList.toggle("no-code", !on);
  }

  function renderTabs() {
    const head = $("#code-tabs");
    head.innerHTML = "";
    if (!codeTabs.length) {
      setCodeShown(false);
      head.innerHTML = '<span class="muted">No code</span>';
      codeEl.innerHTML = pendingQS ? '<p class="empty">These questions have no code references.</p>' : "";
      return;
    }
    for (const t of codeTabs) {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "tab" + (t.path === activeCode ? " active" : "");
      b.title = t.rel;
      b.textContent = t.name;
      if (t.stale) {
        const s = document.createElement("span");
        s.className = "stale";
        s.textContent = " (may be out of date)";
        b.appendChild(s);
      }
      b.addEventListener("click", () => showCode(t.path, true));
      head.appendChild(b);
    }
  }

  function showCode(path, scroll) {
    const t = codeTabs.find((x) => x.path === path) || codeTabs[0];
    if (!t) return;
    activeCode = t.path;
    noCode = false;
    renderTabs();
    setCodeShown(true);
    const meta = '<div class="code-meta"><span class="mono">' + escapeHTML(t.rel) + "</span>" +
      (t.start ? "<span>lines " + t.start + "–" + t.end + "</span>" : "") +
      '<span class="grow"></span><a href="#" class="open-editor" data-kind="code" data-path="' + escapeHTML(t.path) +
      '" data-line="' + (t.start || 0) + '">open in editor</a></div>';
    codeEl.innerHTML = meta + (t.html || "");
    if (scroll) centerHL();
  }

  // Show no file: the current question has no code ref. The tabs of the other
  // questions stay available.
  function showNoCode() {
    noCode = true;
    activeCode = null;
    renderTabs();
    setCodeShown(false);
    if (codeTabs.length) codeEl.innerHTML = '<p class="empty">This question has no code reference.</p>';
  }

  // Scroll a pane so el sits a third of the way down (or centred).
  function scrollIntoPane(pane, el, ratio) {
    const delta = el.getBoundingClientRect().top - pane.getBoundingClientRect().top;
    pane.scrollTop += delta - pane.clientHeight * (ratio || 1 / 3);
  }

  function centerHL() {
    const hl = $(".line.hl", codeEl);
    if (hl) scrollIntoPane(codeEl, hl, 0.5);
    else codeEl.scrollTop = 0;
  }

  async function focusCode(path, start, end) {
    let t = codeTabs.find((x) => x.path === path);
    if (!t || t.start !== start || t.end !== end) {
      try {
        const p = await getJSON(base + "/code?path=" + encodeURIComponent(path) + "&start=" + start + "&end=" + end);
        if (t) Object.assign(t, p); else codeTabs.push(t = p);
      } catch (e) { toast("Cannot open " + path); return; }
    }
    showCode(path, true);
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  // ------------------------------------------------------------ question panel

  function form() { return $("#qs-form"); }

  function questions() { const f = form(); return f ? $$("section.q", f) : []; }

  function answered(sec) {
    const qid = sec.dataset.qid;
    const sel = $$('input[name="sel.' + CSS.escape(qid) + '"]:checked', sec).length > 0;
    const other = $('input[name="other.' + CSS.escape(qid) + '"]', sec);
    return sel || (other && other.value.trim() !== "") || deferred(sec);
  }

  function deferred(sec) {
    const l = $(".later-input", sec);
    return !!l && l.checked;
  }

  function updateSteps() {
    const qs = questions();
    $$(".step", form()).forEach((s, i) => {
      s.classList.toggle("current", !reviewing && i === qIndex);
      s.classList.toggle("done", !!qs[i] && answered(qs[i]));
    });
    const cur = $(".qs-cur", form());
    if (cur) cur.textContent = String(qIndex + 1);
    const pos = $(".qs-pos", form());
    if (pos) pos.hidden = reviewing;
    const done = qs.filter(answered).length;
    const complete = done === qs.length;
    const btn = $('button[data-action="answer"]', form());
    if (btn) {
      btn.disabled = !live;
      btn.classList.toggle("incomplete", !complete);
      const label = qs.length > 1 ? "Send answers" : "Send answer";
      btn.textContent = !complete && qs.length > 1 ? label + " (" + done + "/" + qs.length + ")" : label;
    }
    // Next moves on; on the last question it opens the review once every
    // question has an answer.
    const next = $(".qs-next-btn", form());
    if (next) {
      const last = qIndex >= qs.length - 1;
      next.hidden = reviewing || (last && !complete);
      next.textContent = last ? "Review ›" : "Next ›";
    }
    // Keep a shown hint in step with what is still open.
    const h = $(".qs-hint", form());
    if (h && !h.hidden) showHint(complete ? "" : "Still open: " + openLabels().join(", "));
  }

  function openLabels() {
    return questions().map((sec, i) => (answered(sec) ? null : "Q" + (i + 1))).filter(Boolean);
  }

  // Index of the next unanswered question after `from`, wrapping; -1 if none.
  function nextOpen(from) {
    const qs = questions();
    for (let k = 1; k <= qs.length; k++) {
      const i = (from + k) % qs.length;
      if (!answered(qs[i])) return i;
    }
    return -1;
  }

  function showHint(text) {
    const h = $(".qs-hint", form());
    if (!h) return;
    h.textContent = text;
    h.hidden = !text;
  }

  // Answer only when every question has an answer; otherwise say which are
  // still open and jump to the first one.
  function tryAnswer() {
    const open = openLabels();
    if (!open.length) { post("answer"); return; }
    showHint("Still open: " + open.join(", "));
    showQuestion(nextOpen(-1), true);
  }

  function showQuestion(i, autofocus) {
    const qs = questions();
    if (!qs.length) return;
    reviewing = false;
    const review = $(".review", form());
    if (review) review.hidden = true;
    qIndex = Math.max(0, Math.min(qs.length - 1, i));
    qs.forEach((s, k) => { s.hidden = k !== qIndex; });
    updateSteps();
    if (autofocus) focusQuestionRefs(qs[qIndex]);
  }

  // The answer chosen for one question, as text.
  function answerText(sec) {
    if (deferred(sec)) return "Decide later";
    const qid = sec.dataset.qid;
    const parts = $$('input[name="sel.' + CSS.escape(qid) + '"]:checked', sec).map((i) => i.value);
    const other = $('input[name="other.' + CSS.escape(qid) + '"]', sec);
    if (other && other.value.trim()) parts.push("Other: " + other.value.trim());
    return parts.join(", ");
  }

  // Replace the option list with a summary of every answer. Clicking a
  // question (or its Q button) goes back to it.
  function showReview() {
    const f = form();
    const review = $(".review", f);
    if (!review) return;
    reviewing = true;
    questions().forEach((s) => { s.hidden = true; });
    const items = questions().map((sec, i) => {
      const notes = $(".notes", sec).value.trim();
      return '<li><button type="button" class="review-item" data-step="' + i + '" title="Change this answer">' +
        '<span class="review-q"><span class="muted">Q' + (i + 1) + "</span> " + escapeHTML($(".qtext", sec).textContent.trim()) + "</span>" +
        '<span class="review-a' + (deferred(sec) ? " review-later" : "") + '">→ ' + escapeHTML(answerText(sec)) + (notes ? ' <span class="muted">(' + escapeHTML(notes) + ")</span>" : "") + "</span>" +
        "</button></li>";
    });
    review.innerHTML = '<p class="review-head">All questions answered. Review and send, or click a question to change it.</p>' +
      '<ol class="review-list">' + items.join("") + "</ol>";
    $$(".review-item", review).forEach((b) => b.addEventListener("click", () => showQuestion(+b.dataset.step, true)));
    review.hidden = false;
    showHint("");
    updateSteps();
  }

  function focusQuestionRefs(sec) {
    const doc = $(".chip-doc", sec);
    const code = $(".chip-code", sec);
    // A question with no document keeps the last one, without a highlight.
    $("#doc-note").hidden = !!doc;
    docEl.classList.toggle("no-hl", !doc);
    if (doc) focusDoc(doc.dataset.doc, doc.dataset.ref).then(() => renderQDocTabs(sec));
    else { focusDoc(null, null); activeRef = null; updateJump(); renderQDocTabs(null); }
    if (code) focusCode(code.dataset.path, +code.dataset.start, +code.dataset.end);
    else showNoCode();
  }

  function clipboardText() {
    const f = form();
    const lines = [];
    const title = f.dataset.title;
    if (title) lines.push(title, "");
    questions().forEach((sec, i) => {
      lines.push((i + 1) + ". " + $(".qtext", sec).textContent.trim());
      $$(".opt:not(.other):not(.later)", sec).forEach((o, k) => {
        lines.push("   " + String.fromCharCode(97 + k) + ") " + o.textContent.replace(/^\s*\d+/, "").replace(/\s+/g, " ").trim());
      });
      lines.push("");
    });
    return lines.join("\n").trim() + "\n";
  }

  async function post(action) {
    const f = form();
    if (!f) return;
    if (action === "in-chat") {
      try { await navigator.clipboard.writeText(clipboardText()); toast("Questions copied to the clipboard"); }
      catch (e) { toast("Could not copy to the clipboard"); }
    }
    const body = new URLSearchParams(new FormData(f));
    const r = await fetch(base + "/qs/" + encodeURIComponent(f.dataset.qs) + "/" + action, {
      method: "POST", body, headers: { "Content-Type": "application/x-www-form-urlencoded" },
    });
    if (r.status === 409) toast("Already answered");
    else if (r.status === 410) toast("Claude is disconnected");
    else if (!r.ok) toast(await r.text());
  }

  // Set by the number-key handler so its synthetic click advances even though
  // the form's own keydown listener marked the change as select-only.
  let pickByKey = () => {};

  function initPanel() {
    const f = form();
    if (!f) return;
    f.addEventListener("input", updateSteps);
    f.addEventListener("change", updateSteps);
    f.addEventListener("submit", (e) => { e.preventDefault(); if (live) tryAnswer(); });
    // Choosing an option on a single-choice question moves on to the next
    // unanswered one, after a short pause so the choice is visible. Clicking
    // the radio circle itself, or moving between radios with the arrow keys,
    // only selects: the user moves on with Next.
    let selectOnly = false;
    pickByKey = () => { selectOnly = false; };
    f.addEventListener("pointerdown", (e) => { selectOnly = e.target.matches('input[type="radio"], .later-input'); });
    f.addEventListener("keydown", () => { selectOnly = true; });
    // "Decide later" excludes the options of its question, and the options
    // exclude it.
    f.addEventListener("change", (e) => {
      const t = e.target;
      const sec = t.closest("section.q");
      if (!sec) return;
      if (t.classList.contains("later-input") && t.checked) {
        $$('input[name^="sel."]', sec).forEach((i) => { i.checked = false; });
        const o = $(".other-input", sec);
        if (o) o.value = "";
      } else if (t.name && t.name.startsWith("sel.") && t.checked) {
        const l = $(".later-input", sec);
        if (l) l.checked = false;
      }
      updateSteps();
    });
    f.addEventListener("change", (e) => {
      const t = e.target;
      const pick = (t.type === "radio" && t.name.startsWith("sel.")) || (t.classList.contains("later-input") && t.checked);
      if (!pick) return;
      if (selectOnly) { selectOnly = false; return; }
      const at = qIndex;
      setTimeout(() => {
        if (form() !== f || qIndex !== at || !answered(questions()[at])) return;
        const next = nextOpen(at);
        if (next >= 0) showQuestion(next, true);
        else showReview();
      }, 350);
    });
    $$('button[data-action="in-chat"], button[data-action="dismiss"]', f).forEach((b) =>
      b.addEventListener("click", () => post(b.dataset.action)));
    const prev = $(".qs-prev", f), next = $(".qs-next", f);
    if (prev) prev.addEventListener("click", () => showQuestion(qIndex - 1, true));
    if (next) next.addEventListener("click", () => showQuestion(qIndex + 1, true));
    const nextBtn = $(".qs-next-btn", f);
    if (nextBtn) nextBtn.addEventListener("click", () => {
      if (qIndex >= questions().length - 1) showReview();
      else showQuestion(qIndex + 1, true);
    });
    $$(".step", f).forEach((s) => s.addEventListener("click", () => showQuestion(+s.dataset.step, true)));
    $$(".other-input", f).forEach((inp) => inp.addEventListener("input", () => {
      // Typing Other clears a single-choice selection.
      const sec = inp.closest("section.q");
      if (inp.value && $('input[type="radio"]', sec)) $$('input[type="radio"]', sec).forEach((r) => { r.checked = false; });
      const l = $(".later-input", sec);
      if (inp.value && l) l.checked = false;
    }));
    showQuestion(0, true);
  }

  function setPending(on, ev) {
    document.title = (on ? "● " : "") + baseTitle;
    $("#favicon").href = on ? "/static/favicon-dot.svg" : "/static/favicon.svg";
    $("#pending-dot").hidden = !on;
    if (on && ev) notify(ev.project || "question", "", ev.first || ev.title || "A question is waiting");
  }

  // Browser notification, only while the tab is hidden and permission was
  // given on the index page.
  function notify(title, project, body) {
    if (!("Notification" in window) || Notification.permission !== "granted" || !document.hidden) return;
    const n = new Notification("ai-contextview · " + title + (project ? " · " + project : ""), { body, tag: "ai-contextview-" + ws });
    n.onclick = () => { window.focus(); n.close(); };
  }

  function selectPtab(name) {
    $$(".ptab").forEach((t) => t.classList.toggle("active", t.dataset.ptab === name));
    $("#question").hidden = name !== "question";
    $("#history").hidden = name !== "history";
  }

  // ------------------------------------------------------------ events

  const handlers = {
    state(d) {
      live = d.live;
      document.body.classList.toggle("ended", !d.live);
      panel.classList.toggle("disabled", !d.live);
      $("#ended").hidden = d.live;
      $("#project").textContent = d.project;
      baseTitle = "ai-contextview · " + d.project;
      setPending(d.pending && !!pendingQS);
      $("#done-pill").hidden = !d.done;
      $("#prov-pill").hidden = !d.provisional;
      $("#chat-pill").hidden = !d.in_chat;
      document.title = (d.done ? "✓ " : (d.pending && pendingQS ? "● " : "")) + baseTitle;
      // The spec went away (a picked document cleared when ask mode ended):
      // with no question waiting, drop the document panel.
      if (hasSpec && !d.has_spec && !d.pending) {
        setDocPanel(false);
        setDoc({ path: "", html: "" }, false);
        currentDoc = null;
      }
      hasSpec = !!d.has_spec;
      $('.dtab[data-view="changes"]').hidden = !hasSpec;
      if (!hasSpec && docView === "changes") setView("spec");
      if (hasSpec) setDocPanel(true);
      else if (!sawState && !d.pending) setDocPanel(false);
      sawState = true;
      const oq = $("#oq");
      oq.hidden = !d.open_questions;
      oq.textContent = d.open_questions + " open question" + (d.open_questions === 1 ? "" : "s");
      if (!d.live && pendingQS) $("#panel-summary").textContent = "Claude disconnected";
    },
    artifacts(d) {
      const nav = $("#artifacts");
      nav.innerHTML = d.html;
      nav.hidden = !d.html;
      layout.classList.toggle("has-artifacts", !!d.html);
      $$(".artifact", nav).forEach((a) => {
        a.classList.toggle("current", a.dataset.doc === currentDoc);
        a.addEventListener("click", () => loadDoc(a.dataset.doc));
      });
    },
    doc(d) {
      if (currentDoc === null || d.path === currentDoc) {
        const active = $("mark.q.active, .q-section.active", docEl);
        const ref = active && active.dataset.ref;
        setDoc(d, true);
        if (ref) $$('[data-ref="' + CSS.escape(ref) + '"]', docEl).forEach((m) => m.classList.add("active"));
      }
    },
    code(d) {
      const prev = activeCode;
      codeTabs = d.tabs || [];
      // No tabs (e.g. after spec_done): clear the pane.
      if (!codeTabs.length) { activeCode = null; renderTabs(); return; }
      if (noCode) { showNoCode(); return; }
      if (!codeTabs.some((t) => t.path === prev)) activeCode = codeTabs.length ? codeTabs[0].path : null;
      const keep = codeEl.scrollTop;
      showCode(activeCode, false);
      if (prev === activeCode) codeEl.scrollTop = keep; else centerHL();
    },
    focus(d) {
      if (d.doc) focusDoc(d.doc.path, d.doc.ref);
      if (d.code) focusCode(d.code.path, d.code.start, d.code.end);
    },
    ask(d) {
      const q = $("#question");
      if (d.qs_id) {
        const isNew = d.qs_id !== pendingQS;
        if (isNew) {
          setDocPanel(hasSpec || !!d.doc_panel);
          pendingQS = d.qs_id;
          // A new question must not hide behind a zoomed document.
          setZoom(false);
          q.innerHTML = d.html;
          $("#panel-summary").textContent = "";
          selectPtab("question");
          initPanel();
          setPending(true, d);
        }
      } else {
        pendingQS = "";
        activeRef = null;
        $("#doc-note").hidden = true;
        renderQDocTabs(null);
        updateJump();
        if (!codeTabs.length) renderTabs(); // drop "these questions have no code"
        setPending(false);
        if (d.done) {
          q.innerHTML = '<div class="done-banner"><h3>✓ Spec finished · ' + escapeHTML(d.done_at) + "</h3>" +
            '<p class="muted small">Claude has no more questions. Continue in the chat to change anything. ' +
            '<a href="#" class="review-changes">Review all changes in this session</a></p>' +
            (d.done_html ? '<div class="done-summary">' + d.done_html + "</div>" : "") + "</div>";
          $("#panel-summary").textContent = "Finished · " + d.done_at;
          selectPtab("question");
          if (!doneShown) notify("Spec finished", d.project, "Claude has no more questions.");
          doneShown = true;
          document.title = "✓ " + baseTitle;
        } else {
          doneShown = false;
          q.innerHTML = '<p class="empty">No question right now.</p>';
          $("#panel-summary").textContent = d.summary || "";
        }
      }
    },
    ask_closed() { /* the following ask event clears the panel */ },
    history(d) {
      $("#history").innerHTML = d.html;
      $("#history-count").textContent = d.count ? "(" + d.count + ")" : "";
    },
    redirect(d) { window.location.href = d.url; },
  };

  // ------------------------------------------------------------ presence

  // The page tells the daemon when it is hidden (a closed browser pane keeps
  // its tab) and when someone uses it, so that Claude can say in the chat
  // that questions are waiting when nobody sees them.
  const pageID = (window.crypto && crypto.randomUUID) ? crypto.randomUUID() : String(Math.random()).slice(2);
  function presence(fields) {
    const body = new URLSearchParams(Object.assign({ c: pageID }, fields));
    fetch(base + "/presence", {
      method: "POST", body, headers: { "Content-Type": "application/x-www-form-urlencoded" },
    }).catch(() => {});
  }
  document.addEventListener("visibilitychange", () => presence({ hidden: document.hidden ? "1" : "0" }));
  let lastActive = 0;
  function used() {
    if (document.hidden) return;
    const now = Date.now();
    if (now - lastActive < 5000) return;
    lastActive = now;
    presence({ active: "1" });
  }
  for (const ev of ["pointermove", "pointerdown", "keydown", "wheel", "touchstart"]) {
    window.addEventListener(ev, used, { passive: true, capture: true });
  }

  function connect() {
    const es = new EventSource(base + "/events?c=" + encodeURIComponent(pageID) + (document.hidden ? "&h=1" : ""));
    for (const name of Object.keys(handlers)) {
      es.addEventListener(name, (e) => {
        try { handlers[name](JSON.parse(e.data)); } catch (err) { console.error(name, err); }
      });
    }
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) setTimeout(connect, 2000);
    };
  }

  // ------------------------------------------------------------ UI wiring

  document.addEventListener("click", (e) => {
    const chip = e.target.closest(".chip");
    if (!chip || chip.classList.contains("chip-nodoc")) return;
    if (chip.classList.contains("chip-doc")) focusDoc(chip.dataset.doc, chip.dataset.ref);
    else focusCode(chip.dataset.path, +chip.dataset.start, +chip.dataset.end);
  });

  $$(".ptab").forEach((t) => t.addEventListener("click", () => selectPtab(t.dataset.ptab)));

  $$(".dtab").forEach((t) => t.addEventListener("click", () => setView(t.dataset.view)));
  $("#chg-base").addEventListener("change", (e) => { chgBase = e.target.value; loadChanges(); });
  $$(".seg-btn").forEach((b) => b.addEventListener("click", () => {
    chgMode = b.dataset.mode;
    $$(".seg-btn").forEach((x) => x.classList.toggle("active", x === b));
    loadChanges();
  }));
  document.addEventListener("click", (e) => {
    if (!e.target.closest(".review-changes")) return;
    e.preventDefault();
    chgBase = "start";
    setView("changes");
  });

  $("#doc-jump").addEventListener("click", (e) => {
    e.preventDefault();
    if (activeRef) focusDoc(activeRef.path, activeRef.ref);
  });

  // ------------------------------------------------------------ zoom

  // The zoom button shows only the document pane; clicking it again, or
  // Esc, restores the layout.
  function setZoom(on) {
    layout.classList.toggle("zoom-doc", on);
    const b = $("#doc-zoom");
    b.textContent = on ? "⤡" : "⤢";
    b.setAttribute("aria-pressed", on ? "true" : "false");
    b.title = on ? "Restore the layout (Esc)" : "Show only the document (Esc restores)";
  }
  $("#doc-zoom").addEventListener("click", () => setZoom(!layout.classList.contains("zoom-doc")));
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && layout.classList.contains("zoom-doc")) {
      e.preventDefault();
      setZoom(false);
    }
  });

  document.addEventListener("keydown", (e) => {
    const f = form();
    if (!f) return;
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName) &&
      !/^(radio|checkbox)$/.test(document.activeElement.type || "");
    if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
      e.preventDefault();
      if (live) tryAnswer();
      return;
    }
    if (e.altKey && (e.key === "ArrowLeft" || e.key === "ArrowRight")) {
      e.preventDefault();
      const last = qIndex >= questions().length - 1;
      if (reviewing && e.key === "ArrowLeft") showQuestion(questions().length - 1, true);
      else if (!reviewing && last && e.key === "ArrowRight" && !openLabels().length) showReview();
      else if (!reviewing) showQuestion(qIndex + (e.key === "ArrowLeft" ? -1 : 1), true);
      return;
    }
    if (!reviewing && !typing && !e.ctrlKey && !e.metaKey && !e.altKey && /^[1-6]$/.test(e.key)) {
      const sec = questions()[qIndex];
      const inp = sec && $$('.opt:not(.other):not(.later) input', sec)[+e.key - 1];
      // Number keys work like clicking the option row: select and move on.
      if (inp) { pickByKey(); inp.click(); e.preventDefault(); }
    }
  });

  // Splitters and layout; sizes are a per-browser convenience.
  const root = document.documentElement;
  for (const k of ["split", "panel-h", "panel-side"]) {
    const v = store.get(k, null);
    if (v) root.style.setProperty("--" + k, v);
  }
  if (store.get("layout", "side") === "bottom") layout.classList.add("bottom");
  const theme = store.get("theme", "");
  if (theme) root.dataset.theme = theme;

  function updateDockLabel() {
    $("#dock-toggle").textContent = layout.classList.contains("bottom") ? "Questions beside" : "Questions below";
  }
  updateDockLabel();
  $("#dock-toggle").addEventListener("click", () => {
    layout.classList.toggle("bottom");
    store.set("layout", layout.classList.contains("bottom") ? "bottom" : "side");
    updateDockLabel();
  });
  $("#theme-toggle").addEventListener("click", () => {
    const dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
    root.dataset.theme = dark ? "light" : "dark";
    store.set("theme", root.dataset.theme);
  });

  function drag(el, onMove) {
    el.addEventListener("mousedown", (e) => {
      e.preventDefault();
      const move = (ev) => onMove(ev, layout.getBoundingClientRect());
      const up = () => { document.removeEventListener("mousemove", move); document.removeEventListener("mouseup", up); };
      document.addEventListener("mousemove", move);
      document.addEventListener("mouseup", up);
    });
  }
  drag($("#split-v"), (e, r) => {
    const art = layout.classList.contains("has-artifacts") ? 200 : 0;
    const v = Math.max(200, e.clientX - r.left - art) + "px";
    root.style.setProperty("--split", v);
    store.set("split", v);
  });
  drag($("#split-h"), (e, r) => {
    if (layout.classList.contains("bottom")) {
      const v = Math.max(120, r.bottom - e.clientY) + "px";
      root.style.setProperty("--panel-h", v);
      store.set("panel-h", v);
    } else {
      // Between the questions (top right) and the code (bottom right).
      const v = Math.max(120, Math.min(r.height - 80, e.clientY - r.top)) + "px";
      root.style.setProperty("--panel-side", v);
      store.set("panel-side", v);
    }
  });

  connect();
})();
