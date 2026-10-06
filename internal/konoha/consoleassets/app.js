"use strict";

// Konoha local console SPA (#105). Read-only: it fetches the board + cycle
// detail and never executes artifact content. The browser authenticates with
// the per-process CSRF token issued by /api/v1/status; CLI clients use a bearer
// token instead. No outbound network calls (CSP connect-src 'self').

const state = { csrf: "", csrfHeader: "X-Konoha-CSRF", selected: "" };

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
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
    board.append(el("li", "error", String(err.message || err)));
    return;
  }
  const cycles = data.cycles || [];
  if (cycles.length === 0) {
    board.append(el("li", "empty", "No cycles recorded."));
    return;
  }
  for (const c of cycles) {
    const li = el("li");
    li.setAttribute("aria-selected", c.id === state.selected ? "true" : "false");
    li.append(el("span", "id", c.id));
    const row = el("span", "row");
    if (c.repo) row.append(el("span", "", c.repo + (c.issue ? "#" + c.issue : "")));
    row.append(el("span", "state " + (c.state || ""), c.state || "UNKNOWN"));
    if (c.route) row.append(el("span", "", c.route));
    li.append(row);
    li.addEventListener("click", () => selectCycle(c.id));
    board.append(li);
  }
}

async function selectCycle(id) {
  state.selected = id;
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
  renderDetail(d);
}

function renderDetail(d) {
  const detail = document.getElementById("detail");
  detail.replaceChildren();

  const kv = el("dl", "kv");
  const add = (k, v) => {
    if (v === undefined || v === null || v === "") return;
    kv.append(el("dt", "", k), el("dd", "", String(v)));
  };
  add("id", d.id);
  add("owner", d.owner);
  add("repo", d.repo);
  add("issue", d.issue);
  add("state", d.state);
  add("running", d.running ? "yes" : undefined);
  add("route", d.route);
  add("complexity", d.complexity);
  detail.append(kv);

  if (d.outcome) {
    const o = d.outcome;
    const okv = el("dl", "kv");
    const oadd = (k, v) => {
      if (v === undefined || v === null || v === "") return;
      okv.append(el("dt", "", k), el("dd", "", String(v)));
    };
    oadd("agent", o.agentOutcome);
    oadd("verification", o.verificationOutcome);
    oadd("escalation", o.escalationReason);
    oadd("duration(s)", o.durationSeconds);
    detail.append(el("h3", "", "Outcome"), okv);
  }

  if (d.status && Array.isArray(d.status.conditions) && d.status.conditions.length) {
    const list = el("ul", "artifacts");
    for (const c of d.status.conditions) {
      list.append(el("li", "", c.type + " " + c.status + (c.reason ? " (" + c.reason + ")" : "")));
    }
    detail.append(el("h3", "", "Conditions"), list);
  }

  const arts = el("ul", "artifacts");
  if (!d.artifacts || d.artifacts.length === 0) {
    arts.append(el("li", "empty", "No artifacts."));
  } else {
    for (const a of d.artifacts) arts.append(el("li", "", a));
  }
  detail.append(el("h3", "", "Artifacts"), arts);
}

bootstrap().catch((err) => {
  const app = document.getElementById("app");
  app.replaceChildren(el("p", "error", "console error: " + (err.message || err)));
});
