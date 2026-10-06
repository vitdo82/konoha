"use strict";

// Konoha local console SPA (#106). Read-only: it fetches the board + cycle
// detail and never executes artifact content (everything is rendered via
// textContent, never innerHTML). The browser authenticates with the per-process
// CSRF token issued by /api/v1/status; CLI clients use a bearer token instead.
// No outbound network calls (CSP connect-src 'self').
//
// "Summaries are not proof": agent outcomes and deterministic verification
// outcomes are separate fields. A conflict (agent SUCCESS while verification
// FAILED/ERROR) is highlighted and never collapsed into the agent's prose.

const state = { csrf: "", csrfHeader: "X-Konoha-CSRF", decisions: false, queue: false, selected: "", tab: "result", detail: null, commentDraft: [] };

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function present(v) {
  return v !== undefined && v !== null && v !== "";
}

function kv() {
  const dl = el("dl", "kv");
  return {
    node: dl,
    add(k, v) {
      if (!present(v)) return;
      dl.append(el("dt", "", k), el("dd", "", String(v)));
    },
  };
}

async function api(path) {
  const res = await apiRaw(path);
  return res.json();
}

async function apiRaw(path) {
  const headers = {};
  if (state.csrf) headers[state.csrfHeader] = state.csrf;
  const res = await fetch(path, { headers, credentials: "same-origin" });
  if (!res.ok) throw new Error(path + " -> " + res.status);
  return res;
}

// apiPost writes a mutation (only used by the #108 decision panel). The CSRF
// header is required for the browser; the server also checks the Origin.
async function apiPost(path, body) {
  const headers = { "Content-Type": "application/json" };
  if (state.csrf) headers[state.csrfHeader] = state.csrf;
  const res = await fetch(path, {
    method: "POST", headers, credentials: "same-origin", body: JSON.stringify(body),
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = { error: text }; }
  if (!res.ok) throw new Error((data && data.error) || (path + " -> " + res.status));
  return data;
}

async function bootstrap() {
  const status = await api("/api/v1/status");
  state.csrf = status.csrfToken;
  state.csrfHeader = status.csrfHeader || state.csrfHeader;
  state.decisions = !!status.decisions;
  state.queue = !!status.queue;
  document.getElementById("meta").textContent =
    status.name + " " + status.version + (status.readOnly ? " · read-only" : "");
  if (state.queue) document.getElementById("queue-panel").hidden = false;
  await Promise.all([refreshMetrics(), refreshBoard(), refreshQueue()]);
}

// Metrics dashboard (#112): the existing §39 rate gauges plus throughput and #87
// cost/token figures, projected server-side. A null value is "no data" — it is
// never shown as zero (the server never fabricates one).
async function refreshMetrics() {
  const root = document.getElementById("metrics");
  if (!root) return;
  root.replaceChildren();
  let data;
  try {
    data = await api("/api/v1/metrics");
  } catch (err) {
    root.append(el("p", "error", String(err.message || err)));
    return;
  }
  const grid = el("div", "metric-grid");
  for (const m of data.metrics || []) grid.append(metricCard(m));
  root.append(grid);
}

function metricCard(m) {
  const box = el("div", "metric");
  box.append(el("span", "metric-label", m.label || m.key));
  const has = m.value !== null && m.value !== undefined;
  box.append(el("span", "metric-value" + (has ? "" : " nodata"), has ? formatMetric(m) : "no data"));
  if (present(m.unit)) {
    box.append(el("span", "metric-unit", has ? m.unit : ""));
  }
  return box;
}

function formatMetric(m) {
  const v = Number(m.value);
  if (m.unit === "ratio") return (v * 100).toFixed(1) + "%";
  if (m.unit === "usd") return "$" + v.toFixed(2);
  return Number.isInteger(v) ? String(v) : v.toFixed(2);
}

// Needs-attention queue (#109): everything waiting on a human. retry/cancel go
// through the authoritative channel; attach/pause/resume are disabled without a
// session provider; an interrupted cycle needs an explicit stopped confirmation.
async function refreshQueue() {
  if (!state.queue) return;
  const root = document.getElementById("queue");
  root.replaceChildren();
  let data;
  try {
    data = await api("/api/v1/queue");
  } catch (err) {
    root.append(el("p", "error", String(err.message || err)));
    return;
  }
  const items = data.items || [];
  if (items.length === 0) {
    root.append(el("p", "empty", "Nothing needs attention."));
    return;
  }
  for (const it of items) root.append(queueItem(it));
}

function queueItem(it) {
  const box = el("article", "queue-item queue-" + (it.reason || ""));
  const head = el("div", "queue-head");
  head.append(el("span", "id", it.id));
  head.append(el("span", "badge reason", it.reason || ""));
  if (present(it.state)) head.append(el("span", "state " + it.state, it.state));
  if (it.attempt) head.append(el("span", "muted", "attempt " + it.attempt));
  if (it.dryRun) head.append(el("span", "badge dry", "dry-run"));
  box.append(head);
  if (it.interrupted) {
    box.append(el("p", "warn",
      "Interrupted: the worker/lease was lost. Confirm the old process stopped before retrying."));
  }
  const confirm = el("label", "queue-confirm");
  if (it.requiresConfirmation) {
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.className = "confirm-stopped";
    confirm.append(cb, el("span", "", "process stopped"));
    box.append(confirm);
  }
  const status = el("p", "queue-status", "");
  const actions = el("div", "queue-actions");
  for (const a of it.actions || []) {
    const btn = el("button", "queue-btn " + a.action, a.action);
    if (!a.enabled || it.dryRun) {
      btn.disabled = true;
      if (a.reason) btn.title = a.reason;
      if (it.dryRun) btn.title = "dry run: preview only";
    }
    btn.addEventListener("click", () => submitQueueAction(it, a, confirm, status));
    actions.append(btn);
  }
  box.append(actions, status);
  return box;
}

async function submitQueueAction(it, action, confirmBox, status) {
  const cb = confirmBox.querySelector(".confirm-stopped");
  if (it.requiresConfirmation && cb && !cb.checked) {
    status.textContent = "confirm the old process stopped first";
    return;
  }
  status.textContent = "submitting…";
  try {
    const res = await apiPost("/api/v1/cycles/" + encodeURIComponent(it.id) + "/actions", {
      action: action.action,
      confirmStopped: !!(cb && cb.checked),
    });
    status.textContent = (res && res.outcome) ? res.outcome + (res.reason ? ": " + res.reason : "") : "submitted";
    await refreshQueue();
    await refreshBoard();
  } catch (err) {
    status.textContent = String(err.message || err);
  }
}

async function refreshBoard() {
  const board = document.getElementById("board");
  board.replaceChildren();
  let data;
  try {
    data = await api("/api/v1/cycles");
  } catch (err) {
    board.append(el("p", "error", String(err.message || err)));
    return;
  }
  const columns = data.columns || [];
  if (columns.length === 0) {
    board.append(el("p", "empty", "No cycles recorded."));
    return;
  }
  for (const col of columns) {
    const section = el("section", "column column-" + col.id);
    const head = el("h3", "column-head");
    head.append(el("span", "", col.title), el("span", "count", String(col.count || 0)));
    section.append(head);
    if (!col.cards || col.cards.length === 0) {
      section.append(el("p", "empty", "—"));
    }
    for (const c of col.cards || []) section.append(card(c));
    board.append(section);
  }
}

function card(c) {
  const li = el("article", "card");
  li.setAttribute("aria-selected", c.id === state.selected ? "true" : "false");
  const title = el("div", "card-title");
  title.append(el("span", "id", c.id));
  li.append(title);
  const row = el("div", "card-meta");
  if (c.repo) row.append(el("span", "", c.repo + (c.issue ? "#" + c.issue : "")));
  row.append(el("span", "state " + (c.state || ""), c.state || "UNKNOWN"));
  if (c.route) row.append(el("span", "", c.route));
  if (c.complexity) row.append(el("span", "", "C" + c.complexity));
  if (c.priority) row.append(el("span", "badge priority", c.priority));
  if (c.assignee) row.append(el("span", "badge assignee", "@" + c.assignee));
  if (c.outcomeConflict) row.append(el("span", "badge conflict", "conflict"));
  li.append(row);
  li.addEventListener("click", () => selectCycle(c.id));
  return li;
}

async function selectCycle(id) {
  state.selected = id;
  state.tab = "result";
  state.commentDraft = [];
  await refreshBoard();
  const detail = document.getElementById("detail");
  detail.replaceChildren(el("p", "empty", "Loading…"));
  let d;
  try {
    d = await api("/api/v1/cycles/" + encodeURIComponent(id));
  } catch (err) {
    detail.replaceChildren(el("p", "error", String(err.message || err)));
    return;
  }
  state.detail = d;
  renderDetail();
}

function renderDetail() {
  const d = state.detail;
  if (!d) return;
  renderTabs(d);
  const detail = document.getElementById("detail");
  detail.replaceChildren();
  const tabs = {
    result: tabResult, diff: tabDiff, validation: tabValidation,
    artifacts: tabArtifacts, metadata: tabMetadata,
  };
  (tabs[state.tab] || tabResult)(detail, d);
}

function renderTabs(d) {
  const bar = document.getElementById("tabs");
  bar.replaceChildren();
  const defs = [
    ["result", "Result", true],
    ["diff", "Diff", !!d.diff],
    ["validation", "Validation", !!d.validation],
    ["artifacts", "Artifacts", true],
    ["metadata", "Metadata", true],
  ];
  for (const [id, label, show] of defs) {
    if (!show) continue;
    const b = el("button", "tab" + (state.tab === id ? " active" : ""), label);
    b.setAttribute("role", "tab");
    b.setAttribute("aria-selected", state.tab === id ? "true" : "false");
    b.addEventListener("click", () => {
      state.tab = id;
      renderDetail();
    });
    bar.append(b);
  }
}

// Result tab: the deterministic proof, never an agent summary. agentOutcome and
// verificationOutcome are separate fields; a conflict is called out explicitly.
function tabResult(root, d) {
  const o = d.outcome || {};
  const st = d.status || {};
  const agent = o.agentOutcome || st.agentOutcome || "";
  const verification = o.verificationOutcome || st.verificationOutcome || "";

  const outcome = el("div", "outcomes");
  const field = (label, value) => {
    const box = el("div", "outcome-field");
    box.append(el("span", "outcome-label", label));
    box.append(el("span", "outcome-value " + value, value || "—"));
    return box;
  };
  outcome.append(field("agent", agent), field("verification", verification));
  root.append(el("h3", "", "Deterministic outcomes"), outcome);

  if (d.outcomeConflict) {
    root.append(el("p", "conflict-note",
      "Conflict: the agent reported SUCCESS but verification did not pass. Summaries are not proof."));
  }

  const info = kv();
  info.add("terminal state", d.state);
  if (present(o.escalationReason)) info.add("escalation", o.escalationReason);
  if (present(o.agentMessage)) info.add("agent message", o.agentMessage);
  if (present(o.durationSeconds)) info.add("duration (s)", o.durationSeconds);
  root.append(el("h3", "", "Result"), info.node);

  const conds = (o.conditions && o.conditions.length ? o.conditions : st.conditions) || [];
  const list = el("ol", "conditions");
  if (conds.length === 0) {
    list.append(el("li", "empty", "No conditions recorded."));
  } else {
    for (const c of conds) {
      const li = el("li", "");
      li.append(el("span", "cond-type", c.type || ""));
      li.append(el("span", "cond-status " + (c.status || ""), c.status || ""));
      if (present(c.reason)) li.append(el("span", "cond-reason", c.reason));
      if (present(c.message)) li.append(el("span", "cond-message", c.message));
      list.append(li);
    }
  }
  root.append(el("h3", "", "Conditions (in order)"), list);
  decisionPanel(root, d);
}

// Decision panel (#108): approve / request-changes / deny. It is shown ONLY for
// an open gate bound to the exact attempt+SHA the detail view rendered, so a
// stale submission cannot be made from a superseded view. Nothing here is a
// second source of truth — the server writes the §18 decision channel.
function decisionPanel(root, d) {
  if (!state.decisions || !d.gate || !d.gate.open) return;
  const gate = d.gate;
  const box = el("section", "decisions");
  box.append(el("h3", "", "Decision"));
  box.append(el("p", "muted",
    "Gate bound to attempt " + gate.attempt + " @ " + String(gate.sha || "").slice(0, 12)));

  const feedback = document.createElement("textarea");
  feedback.rows = 3;
  feedback.placeholder = "Required for request changes";
  box.append(feedback);

  if (state.commentDraft.length) {
    box.append(el("p", "muted",
      state.commentDraft.length + " anchored comment(s) staged (see the Diff tab)"));
  }

  const status = el("p", "decision-status", "");
  const submit = async (action) => {
    if (action === "request-changes" && !feedback.value.trim()) {
      status.textContent = "request-changes requires feedback";
      return;
    }
    status.textContent = "submitting…";
    const comments = action === "request-changes" && state.commentDraft.length
      ? state.commentDraft.map((c) => ({ file: c.file, line: c.line, side: c.side, sha: gate.sha, body: c.body }))
      : undefined;
    try {
      await apiPost("/api/v1/cycles/" + encodeURIComponent(d.id) + "/decisions", {
        action, attempt: gate.attempt, sha: gate.sha,
        feedback: action === "request-changes" ? feedback.value : undefined,
        comments,
      });
      if (action === "request-changes") state.commentDraft = [];
      status.textContent = "submitted";
      await selectCycle(d.id);
    } catch (err) {
      status.textContent = String(err.message || err);
    }
  };
  const actions = el("div", "decision-actions");
  for (const [action, label] of [
    ["approve", "Approve"], ["request-changes", "Request changes"], ["deny", "Deny"],
  ]) {
    const b = el("button", "decision-btn " + action, label);
    b.addEventListener("click", () => submit(action));
    actions.append(b);
  }
  box.append(actions, status);
  root.append(box);
}

// Diff tab: changed files + patch, with line-anchored comment threads and the
// #113 capture control. When a gate is open, a reviewer can attach a comment to
// any added/context line; staged drafts are submitted with a request-changes
// decision and anchored to the gate's reviewed SHA (never a parallel store).
function tabDiff(root, d) {
  const view = d.diff;
  if (!view) {
    root.append(el("p", "empty", "No diff recorded."));
    return;
  }
  const gate = d.gate || {};
  const canComment = !!(state.decisions && gate.open);
  const threads = {};
  for (const t of view.threads || []) {
    (threads[t.file + "|" + t.line] ||= []).push(t);
  }
  const drafts = {};
  for (const c of state.commentDraft) {
    (drafts[c.file + "|" + c.line] ||= []).push(c);
  }
  if (canComment) {
    root.append(el("p", "muted",
      "Click “＋ comment” on a line to anchor feedback to the reviewed revision."));
  }
  if (!view.files || view.files.length === 0) {
    root.append(el("p", "empty", "No changed files."));
  }
  for (const f of view.files) {
    const box = el("section", "difffile");
    const head = el("div", "difffile-head");
    head.append(el("span", "path", f.path || "(unknown)"));
    head.append(el("span", "stat add", "+" + (f.added || 0)));
    head.append(el("span", "stat del", "-" + (f.removed || 0)));
    box.append(head);
    const pre = el("pre", "patch");
    let newLine = 0;
    for (const line of f.lines || []) {
      const m = /^@@ -\d+(?:,\d+)? \+(\d+)/.exec(line);
      if (m) {
        newLine = parseInt(m[1], 10);
        pre.append(el("div", "patch-line hunk", line));
        continue;
      }
      const removed = line.startsWith("-");
      let cls = "patch-line";
      if (line.startsWith("+")) cls += " add";
      else if (removed) cls += " del";
      pre.append(el("div", cls, line));
      const anchor = newLine;
      if (!removed) newLine++;
      const key = f.path + "|" + anchor;
      if (threads[key]) {
        for (const t of threads[key]) pre.append(threadBox(t));
        delete threads[key];
      }
      if (drafts[key]) {
        for (const c of drafts[key]) pre.append(draftBox(c));
        delete drafts[key];
      }
      // Only added/context lines have a new-side anchor; a removed line has none.
      if (canComment && !removed) {
        pre.append(addCommentControl(f.path, anchor));
      }
    }
    box.append(pre);
    root.append(box);
  }
  // Any thread/draft whose line did not resolve is still shown (never silently lost).
  const leftover = Object.keys(threads).concat(Object.keys(drafts).filter((k) => !threads[k]));
  if (leftover.length) {
    const box = el("section", "difffile");
    box.append(el("div", "difffile-head", "Unanchored comments"));
    for (const k of leftover) {
      for (const t of threads[k] || []) box.append(threadBox(t));
      for (const c of drafts[k] || []) box.append(draftBox(c));
    }
    root.append(box);
  }
}

// addCommentControl is the #113 capture affordance: it stages a draft comment for
// this file:line in state.commentDraft (nothing is persisted until a
// request-changes submission). Drafts are plain data, never a second store.
function addCommentControl(file, line) {
  const wrap = el("div", "comment-add");
  const btn = el("button", "comment-add-btn", "＋ comment");
  btn.type = "button";
  btn.addEventListener("click", () => {
    if (wrap.querySelector("textarea")) return;
    const ta = document.createElement("textarea");
    ta.rows = 2;
    ta.placeholder = "Comment on " + file + ":" + line;
    const save = el("button", "comment-save", "Add");
    save.type = "button";
    save.addEventListener("click", () => {
      const body = ta.value.trim();
      if (!body) return;
      state.commentDraft.push({ file, line, side: "new", body });
      renderDetail();
    });
    const cancel = el("button", "comment-cancel", "Cancel");
    cancel.type = "button";
    cancel.addEventListener("click", () => wrap.replaceChildren(btn));
    wrap.append(ta, save, cancel);
    ta.focus();
  });
  wrap.append(btn);
  return wrap;
}

// draftBox renders one staged (not-yet-submitted) comment.
function draftBox(c) {
  const box = el("div", "thread draft");
  box.append(el("div", "thread-anchor", c.file + ":" + c.line + " (draft)"));
  const item = el("div", "comment");
  item.append(el("span", "comment-author", "you"));
  item.append(el("span", "comment-body", c.body || ""));
  box.append(item);
  return box;
}

function threadBox(t) {
  const box = el("div", "thread");
  box.append(el("div", "thread-anchor", t.file + ":" + t.line + (t.side ? " (" + t.side + ")" : "")));
  const current = state.detail && state.detail.gate && state.detail.gate.sha;
  for (const c of t.comments || []) {
    // A comment anchored to a superseded SHA is stale: it is retained for the
    // record but visibly marked and cannot be re-submitted (server refuses it).
    const stale = !!(current && c.sha && c.sha !== current);
    const item = el("div", "comment" + (stale ? " stale" : ""));
    item.append(el("span", "comment-author", c.author || "reviewer"));
    item.append(el("span", "comment-body", c.body || ""));
    if (stale) item.append(el("span", "comment-stale", "stale"));
    box.append(item);
  }
  return box;
}

// Validation tab: the deterministic overall outcome + per-gate streams.
function tabValidation(root, d) {
  const v = d.validation;
  if (!v) {
    root.append(el("p", "empty", "No validation recorded."));
    return;
  }
  const info = kv();
  info.add("verification outcome", v.outcome);
  info.add("mutated tracked files", d.outcome && d.outcome.verificationMutated ? "yes" : undefined);
  root.append(el("h3", "", "Validation"), info.node);
  if (present(v.output)) {
    const pre = el("pre", "validation");
    pre.append(el("div", "", v.output));
    root.append(el("h3", "", "Combined output"), pre);
  }
  for (const g of v.gates || []) {
    const gbox = el("section", "gate");
    gbox.append(el("h4", "", g.id));
    if (present(g.stdout)) {
      const pre = el("pre", "validation");
      pre.append(el("div", "", g.stdout));
      gbox.append(el("span", "stream-label", "stdout"), pre);
    }
    if (present(g.stderr)) {
      const pre = el("pre", "validation");
      pre.append(el("div", "", g.stderr));
      gbox.append(el("span", "stream-label", "stderr"), pre);
    }
    root.append(gbox);
  }
}

// Artifacts tab (#107): list every artifact with size/type; preview text (and
// HTML, as TEXT — never innerHTML) up to the server cap; download anything.
function artifactURL(id, name, download) {
  const seg = String(name).split("/").map(encodeURIComponent).join("/");
  return "/api/v1/cycles/" + encodeURIComponent(id) + "/" +
    (download ? "download/" : "artifacts/") + seg;
}

function formatBytes(n) {
  if (!n) return "0 B";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KiB";
  return (n / (1024 * 1024)).toFixed(1) + " MiB";
}

async function tabArtifacts(root, d) {
  root.append(el("h3", "", "Artifacts"));
  const list = el("ul", "artifacts");
  const preview = el("div", "artifact-preview");
  root.append(list, preview);
  let data;
  try {
    data = await api("/api/v1/cycles/" + encodeURIComponent(d.id) + "/artifacts");
  } catch (err) {
    list.append(el("li", "error", String(err.message || err)));
    return;
  }
  const items = data.artifacts || [];
  if (items.length === 0) list.append(el("li", "empty", "No artifacts recorded."));
  for (const a of items) {
    const li = el("li", "");
    li.append(el("span", "artifact-path", a.path));
    li.append(el("span", "artifact-size", formatBytes(a.size)));
    li.append(el("span", "badge", a.kind));
    if (a.previewable) {
      const pv = el("button", "artifact-action", "Preview");
      pv.addEventListener("click", () => previewArtifact(d.id, a, preview));
      li.append(pv);
    }
    const dl = el("button", "artifact-action", "Download");
    dl.addEventListener("click", () => downloadArtifact(d.id, a));
    li.append(dl);
    list.append(li);
  }
}

async function previewArtifact(id, a, target) {
  target.replaceChildren(el("p", "empty", "Loading…"));
  try {
    const res = await apiRaw(artifactURL(id, a.path, false));
    if (a.kind === "image") {
      const url = URL.createObjectURL(await res.blob());
      const img = el("img", "artifact-image");
      img.src = url;
      img.alt = a.path;
      target.replaceChildren(el("div", "artifact-head", a.path), img);
      return;
    }
    const text = await res.text();
    const pre = el("pre", "artifact-text");
    pre.textContent = text; // untrusted content is text, never innerHTML
    target.replaceChildren(el("div", "artifact-head", a.path), pre);
  } catch (err) {
    target.replaceChildren(el("p", "error", String(err.message || err)));
  }
}

async function downloadArtifact(id, a) {
  try {
    const res = await apiRaw(artifactURL(id, a.path, true));
    const url = URL.createObjectURL(await res.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = String(a.path).split("/").pop();
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  } catch (err) {
    console.error("download failed", err);
  }
}

// Metadata tab: identity, harness/model/digest, usage/cost, attempt number.
function tabMetadata(root, d) {
  const o = d.outcome || {};
  const info = kv();
  info.add("owner", d.owner);
  info.add("repo", d.repo);
  info.add("issue", d.issue);
  info.add("workflow id", d.id);
  info.add("state", d.state);
  info.add("running", d.running ? "yes" : undefined);
  info.add("route", d.route);
  info.add("complexity", d.complexity);
  info.add("attempts", o.attempts);
  info.add("review rounds", o.reviewRounds);
  info.add("policy digest", o.policyDigest);
  root.append(el("h3", "", "Cycle"), info.node);

  const u = o.usage || {};
  const usage = kv();
  usage.add("tokens in", u.tokensIn);
  usage.add("tokens out", u.tokensOut);
  usage.add("cost (usd)", u.costUSD);
  if (o.resources && o.resources.budget) usage.add("budget", o.resources.budget.name);
  root.append(el("h3", "", "Usage"), usage.node);

  const models = (o.resources && o.resources.models) || [];
  if (models.length) {
    const list = el("ul", "models");
    for (const m of models) {
      list.append(el("li", "", (m.stage || "?") + ": " +
        [m.provider, m.model].filter(Boolean).join("/") +
        (m.digest ? " · " + m.digest.slice(0, 12) : "")));
    }
    root.append(el("h3", "", "Models"), list);
  }

  const acts = d.activations || [];
  if (acts.length) {
    const list = el("ul", "models");
    for (const a of acts) {
      list.append(el("li", "", "attempt " + a.attempt + " · " + (a.reason || "") +
        (a.stage ? " · " + a.stage : "") + (a.harness ? " · " + a.harness : "") +
        (a.model ? " · " + a.model : "")));
    }
    root.append(el("h3", "", "Activations"), list);
  }
}

bootstrap().catch((err) => {
  const app = document.getElementById("app");
  app.replaceChildren(el("p", "error", "console error: " + (err.message || err)));
});
