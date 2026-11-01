// ClusterView UI. Plain DOM, no framework or build step.
//
// Polling: one loop fetches /api/v1/nodes (summary + nodes from a single server
// build) every 5s with If-None-Match, and the selected node's pods while one is
// selected. A 304 means "unchanged", so nothing is re-rendered. Data is only ever
// inserted with textContent, never innerHTML, so names can't inject markup.
"use strict";

const POLL_MS = 5000;
const PHASES = ["Running", "Pending", "Succeeded", "Failed", "Unknown"];

const state = {
  snap: null,          // last /api/v1/nodes body
  etag: null,
  lastOk: null,        // Date of the last 200 or 304
  error: null,         // message while the API is unreachable
  selected: null,      // selected node name
  pods: null,          // { etag, data, lastSeen, gone, error }
};

const $ = (id) => document.getElementById(id);

function el(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

// fetchJSON returns {status, etag, body}. body is null on 304. cache "no-store"
// makes the browser pass the 304 through to us instead of answering from its own
// cache with a 200, which would defeat "re-render only on 200".
async function fetchJSON(url, etag) {
  const headers = etag ? { "If-None-Match": etag } : {};
  const res = await fetch(url, { cache: "no-store", headers });
  if (res.status === 304) return { status: 304, etag, body: null };
  let body = null;
  try { body = await res.json(); } catch (_) { /* non-JSON error page */ }
  if (!res.ok) {
    const msg = body && body.error ? body.error : `HTTP ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return { status: res.status, etag: res.headers.get("ETag"), body };
}

// ---- formatting ----

function cores(milli) {
  const c = milli / 1000;
  return Number.isInteger(c) ? String(c) : c.toFixed(2).replace(/0+$/, "");
}

function gib(bytes) {
  return (bytes / 1024 ** 3).toFixed(1);
}

function timeOf(d) {
  return d ? d.toLocaleTimeString() : "never";
}

// ---- rendering ----

function renderSummary() {
  const box = $("summary");
  box.replaceChildren();
  if (!state.snap) {
    box.textContent = "Loading…";
    return;
  }
  const s = state.snap.summary;
  const item = (label, value) => {
    const span = el("span", "", label + " ");
    span.append(el("b", "", String(value)));
    return span;
  };
  box.append(item("Nodes ready", `${s.readyNodes}/${s.nodeCount}`));
  box.append(item("Pods", s.podCount));
  // Known phases first in a fixed order, then anything unexpected.
  const phases = Object.keys(s.podPhases || {});
  const ordered = PHASES.filter((p) => phases.includes(p))
    .concat(phases.filter((p) => !PHASES.includes(p)).sort());
  for (const p of ordered) box.append(item(p || "(no phase)", s.podPhases[p]));
  if (s.unscheduledPods) box.append(item("Unscheduled", s.unscheduledPods));
}

function renderNodes() {
  const grid = $("nodes");
  grid.replaceChildren();
  if (!state.snap) return;
  if (state.snap.nodes.length === 0) {
    grid.append(el("p", "muted", "No nodes."));
    return;
  }
  for (const n of state.snap.nodes) {
    const card = el("button", "node" + (n.name === state.selected ? " selected" : ""));
    card.type = "button";
    card.addEventListener("click", () => selectNode(n.name));

    const head = el("div", "node-head");
    head.append(el("span", "node-name", n.name));
    head.append(el("span", "badge " + n.status, n.status));
    if (!n.schedulable) head.append(el("span", "badge cordoned", "Cordoned"));
    card.append(head);

    const dl = el("dl", "metrics");
    const row = (k, v) => { dl.append(el("dt", "", k), el("dd", "", v)); };
    row("CPU", `${cores(n.allocatable.cpuMilli)} / ${cores(n.capacity.cpuMilli)} cores`);
    row("Memory", `${gib(n.allocatable.memoryBytes)} / ${gib(n.capacity.memoryBytes)} GiB`);
    row("Pods", `${n.podCount} / ${n.allocatable.pods}`);
    card.append(dl);
    card.title = "allocatable / capacity; click to list pods";

    grid.append(card);
  }
}

function renderPods() {
  const panel = $("pods");
  const p = state.pods;
  if (!state.selected || !p) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;
  $("pods-title").textContent = `Pods on ${state.selected}`;

  const banner = $("pods-banner");
  if (p.gone) {
    banner.textContent = `Node ${state.selected} no longer exists (last seen ${timeOf(p.lastSeen)}).`;
  } else if (p.error) {
    banner.textContent = `Couldn't load pods: ${p.error}. Showing data from ${timeOf(p.lastSeen)}, retrying.`;
  }
  banner.hidden = !(p.gone || p.error);
  $("pods-body").classList.toggle("stale", Boolean(p.gone || p.error));

  const body = $("pods-body");
  body.replaceChildren();
  const pods = p.data ? p.data.pods : [];
  for (const pod of pods) {
    const tr = el("tr");
    tr.append(el("td", "", pod.namespace), el("td", "", pod.name), el("td", "", pod.phase || "-"));
    tr.append(el("td", "num" + (pod.restarts > 0 ? " restarts-high" : ""), String(pod.restarts)));
    body.append(tr);
  }
  $("pods-empty").hidden = !p.data || pods.length > 0;
}

function renderStatus() {
  const banner = $("banner");
  if (state.error) {
    banner.textContent = state.lastOk
      ? `Can't reach the API (${state.error}). Showing data from ${timeOf(state.lastOk)}, retrying.`
      : `Can't reach the API (${state.error}), retrying.`;
  }
  banner.hidden = !state.error;
  $("nodes").classList.toggle("stale", Boolean(state.error));
  $("summary").classList.toggle("stale", Boolean(state.error));
  renderUpdated();
}

function renderUpdated() {
  if (!state.lastOk) {
    $("updated").textContent = "";
    return;
  }
  const secs = Math.round((Date.now() - state.lastOk) / 1000);
  $("updated").textContent = `Updated ${secs}s ago`;
}

// ---- data ----

async function pollNodes() {
  try {
    const r = await fetchJSON("/api/v1/nodes", state.etag);
    state.lastOk = new Date();
    state.error = null;
    if (r.status === 200) {
      state.snap = r.body;
      state.etag = r.etag;
      renderSummary();
      renderNodes();
    }
    // The selected node vanished from the cluster: freeze its pod list.
    if (state.selected && state.pods && !state.pods.gone &&
        !state.snap.nodes.some((n) => n.name === state.selected)) {
      state.pods.gone = true;
      renderPods();
    }
  } catch (e) {
    state.error = e.message || "network error";
  }
  renderStatus();
}

async function pollPods() {
  const name = state.selected;
  const p = state.pods;
  if (!name || !p || p.gone) return;
  try {
    const r = await fetchJSON(`/api/v1/nodes/${encodeURIComponent(name)}/pods`, p.etag);
    if (state.selected !== name) return; // selection changed while in flight
    p.lastSeen = new Date();
    p.error = null;
    if (r.status === 200) {
      p.data = r.body;
      p.etag = r.etag;
    }
  } catch (e) {
    if (state.selected !== name) return;
    if (e.status === 404) p.gone = true;
    else p.error = e.message || "network error";
  }
  renderPods();
}

function selectNode(name) {
  if (state.selected === name) return;
  state.selected = name;
  state.pods = { etag: null, data: null, lastSeen: null, gone: false, error: null };
  renderNodes();
  renderPods();
  pollPods();
}

function closePods() {
  state.selected = null;
  state.pods = null;
  renderNodes();
  renderPods();
}

// ---- loop ----

let timer = null;
let running = false;

// tick runs one poll, then schedules the next only after this one finishes, so a
// slow API never piles up overlapping requests (setInterval would).
async function tick() {
  if (running) return;
  running = true;
  clearTimeout(timer);
  await pollNodes();
  await pollPods();
  running = false;
  // Hidden tabs stop polling; visibilitychange restarts it.
  if (!document.hidden) timer = setTimeout(tick, POLL_MS);
}

document.addEventListener("visibilitychange", () => {
  if (document.hidden) clearTimeout(timer);
  else tick();
});

$("pods-close").addEventListener("click", closePods);
setInterval(renderUpdated, 1000);
tick();
