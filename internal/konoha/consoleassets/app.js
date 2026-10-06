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

const state = { csrf: "", csrfHeader: "X-Konoha-CSRF", selected: "", tab: "result", detail: null };

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
  const headers = {};
  if (state.csrf) headers[state.csrfHeader] = state.csrf;
  const res = await fetch(path, { headers, credentials: "same-origin" });
  if (!res.ok) throw new Error(path + " -> " + res.status);
  return res.json();
}

async function bootstrap() {
  const status = await api("/api/v1/status");
  state.csrf = status.csrfToken;
  state.csrfHeader = status.csrfHeader || state.csrfHeader;
  document.getElementById("meta").textContent =
    status.name + " " + status.version + (status.readOnly ? " · read-only" : "");
  await refreshBoard();
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
  const tabs = { result: tabResult, diff: tabDiff, validation: tabValidation, metadata: tabMetadata };
  (tabs[state.tab] || tabResult)(detail, d);
}

function renderTabs(d) {
  const bar = document.getElementById("tabs");
  bar.replaceChildren();
  const defs = [
    ["result", "Result", true],
    ["diff", "Diff", !!d.diff],
    ["validation", "Validation", !!d.validation],
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
}

// Diff tab: changed files + patch, with line-anchored comment threads.
function tabDiff(root, d) {
  const view = d.diff;
  if (!view) {
    root.append(el("p", "empty", "No diff recorded."));
    return;
  }
  const threads = {};
  for (const t of view.threads || []) {
    (threads[t.file + "|" + t.line] ||= []).push(t);
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
      let cls = "patch-line";
      if (line.startsWith("+")) cls += " add";
      else if (line.startsWith("-")) cls += " del";
      pre.append(el("div", cls, line));
      const anchor = newLine;
      if (!line.startsWith("-")) newLine++;
      const key = f.path + "|" + anchor;
      if (threads[key]) {
        for (const t of threads[key]) pre.append(threadBox(t));
        delete threads[key];
      }
    }
    box.append(pre);
    root.append(box);
  }
  // Any thread whose line did not resolve is still shown (never silently lost).
  const leftover = Object.keys(threads);
  if (leftover.length) {
    const box = el("section", "difffile");
    box.append(el("div", "difffile-head", "Unanchored comments"));
    for (const k of leftover) for (const t of threads[k]) box.append(threadBox(t));
    root.append(box);
  }
}

function threadBox(t) {
  const box = el("div", "thread");
  box.append(el("div", "thread-anchor", t.file + ":" + t.line + (t.side ? " (" + t.side + ")" : "")));
  for (const c of t.comments || []) {
    const item = el("div", "comment");
    item.append(el("span", "comment-author", c.author || "reviewer"));
    item.append(el("span", "comment-body", c.body || ""));
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
