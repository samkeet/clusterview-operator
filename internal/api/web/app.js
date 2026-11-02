// ClusterView UI. Plain DOM, no framework or build step.
//
// Polling: one loop fetches /api/v1/nodes (summary + nodes from a single server
// build) every 5s with If-None-Match, and the selected node's pods while one is
// selected. A 304 means "unchanged", so nothing is re-rendered. Data is only ever
// inserted with textContent, never innerHTML, so names can't inject markup.
//
// Clicking a pod's name opens a drawer with Events / Logs / Details tabs. Only
// the active tab polls (see TABS), so a tab that isn't shown costs nothing.
// Events: only page 1 refreshes. On an older page the list stays put, and page
// 1 is still polled with its ETag so a "new events" note can appear.
"use strict";

const POLL_MS = 5000;
const EVENTS_PAGE = 5;
const PHASES = ["Running", "Pending", "Succeeded", "Failed", "Unknown"];

const state = {
  snap: null,          // last /api/v1/nodes body
  etag: null,
  lastOk: null,        // Date of the last 200 or 304
  error: null,         // message while the API is unreachable
  selected: null,      // selected node name
  pods: null,          // { etag, data, lastSeen, gone, error }
  drawer: null,        // open pod drawer, see openDrawer
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

// ago formats the time since d compactly: "40s", "3m", "2h", "5d".
function ago(d) {
  const s = Math.max(0, Math.round((Date.now() - d) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

function phaseClass(phase) {
  if (phase === "Running" || phase === "Succeeded") return "chip ok";
  if (phase === "Failed") return "chip bad";
  if (phase === "Pending") return "chip warn";
  return "chip";
}

function samePod(a, b) {
  return a && b && a.namespace === b.namespace && a.name === b.name;
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
    const tr = el("tr", state.drawer && samePod(state.drawer, pod) ? "sel" : "");
    const open = el("button", "pod-link", pod.name);
    open.type = "button";
    open.dataset.namespace = pod.namespace;
    open.dataset.name = pod.name;
    open.addEventListener("click", () => openDrawer(pod));
    const nameCell = el("td");
    nameCell.append(open);
    tr.append(el("td", "", pod.namespace), nameCell, el("td", "", pod.phase || "-"));
    tr.append(el("td", "num" + (pod.restarts > 0 ? " restarts-high" : ""), String(pod.restarts)));
    body.append(tr);
  }
  $("pods-empty").hidden = !p.data || pods.length > 0;
  syncDrawerPod();
}

// ---- pod drawer ----

// TABS is the drawer's tab registry. render draws the panel from state; poll
// (optional) refreshes it and runs on every tick while that tab is active.
// Logs plug in here as the next feature.
const TABS = {
  events: { render: renderEvents, poll: pollEvents },
  logs: { render: () => {}, poll: null },
  details: { render: renderDetails, poll: null },
};
const TAB_ORDER = ["events", "logs", "details"];

function openDrawer(pod) {
  if (state.drawer && samePod(state.drawer, pod)) {
    $("drawer-close").focus();
    return;
  }
  state.drawer = {
    namespace: pod.namespace,
    name: pod.name,
    pod,             // latest PodInfo from the pods poll
    gone: false,     // the pod left the node's pod list
    tab: "events",
    ev: {
      page: 0,           // 0-based page shown
      data: null,        // response for the page shown
      page1: null,       // { etag, data }: latest page 1, polled even when not shown
      newEvents: false,  // page 1 changed while an older page was shown
      expanded: new Set(), // event names whose message is expanded
      loading: false,
      error: null,
    },
  };
  $("drawer").hidden = false;
  renderDrawer();
  renderPods(); // row highlight
  $("drawer-close").focus();
  pollDrawer();
}

// closeDrawer hides the drawer. With restoreFocus, focus goes back to the pod's
// name button (found again, since polling rebuilds the table).
function closeDrawer(restoreFocus) {
  const d = state.drawer;
  if (!d) return;
  state.drawer = null;
  $("drawer").hidden = true;
  renderPods();
  if (!restoreFocus) return;
  for (const b of $("pods-body").querySelectorAll("button.pod-link")) {
    if (b.dataset.namespace === d.namespace && b.dataset.name === d.name) {
      b.focus();
      return;
    }
  }
}

// syncDrawerPod refreshes the drawer header from the latest pods poll. The pod
// is only marked gone when the list loaded fine and it isn't in it.
function syncDrawerPod() {
  const d = state.drawer;
  const p = state.pods;
  if (!d || !p || !p.data || p.error || p.gone) return;
  const pod = p.data.pods.find((x) => samePod(x, d));
  const gone = !pod;
  if (gone === d.gone && (!pod || JSON.stringify(pod) === JSON.stringify(d.pod))) return;
  d.gone = gone;
  if (pod) d.pod = pod;
  renderDrawerHeader();
  if (d.tab === "details") renderDetails();
}

function renderDrawer() {
  renderDrawerHeader();
  renderTabs();
  TABS[state.drawer.tab].render();
}

function renderDrawerHeader() {
  const d = state.drawer;
  const pod = d.pod;
  $("drawer-title").textContent = d.name;
  const facts = $("drawer-facts");
  facts.replaceChildren(
    el("span", "chip", d.namespace),
    el("span", phaseClass(pod.phase), pod.phase || "No phase"),
    el("span", pod.restarts > 0 ? "chip bad" : "chip", `${pod.restarts} restart${pod.restarts === 1 ? "" : "s"}`),
  );
  if (pod.nodeName) facts.append(el("span", "chip", pod.nodeName));
  const banner = $("drawer-banner");
  banner.textContent = d.gone ? "This pod no longer exists. Its events stay readable until they expire." : "";
  banner.hidden = !d.gone;
}

// renderTabs applies the WAI-ARIA tabs pattern: one tab in the tab order
// (roving tabindex), aria-selected on the active one, other panels hidden.
function renderTabs() {
  const active = state.drawer.tab;
  for (const t of $("drawer-tabs").querySelectorAll("[role=tab]")) {
    const on = t.dataset.tab === active;
    t.setAttribute("aria-selected", String(on));
    t.tabIndex = on ? 0 : -1;
  }
  for (const panel of document.querySelectorAll("#drawer [role=tabpanel]")) {
    panel.hidden = panel.dataset.tab !== active;
  }
}

function setTab(name) {
  const d = state.drawer;
  if (!d || d.tab === name) return;
  d.tab = name;
  renderTabs();
  TABS[name].render();
  renderPager(); // the pager belongs to the Events tab only
  pollDrawer();
}

function renderDetails() {
  const d = state.drawer;
  const pod = d.pod;
  const created = pod.createdAt ? new Date(pod.createdAt) : null;
  const rows = [
    ["Namespace", d.namespace],
    ["Name", d.name],
    ["Node", pod.nodeName || "Not scheduled"],
    ["Phase", pod.phase || "-"],
    ["Restarts", String(pod.restarts)],
    ["Created", created ? `${created.toLocaleString()} (${ago(created)} ago)` : "-"],
  ];
  const dl = $("details");
  dl.replaceChildren();
  for (const [k, v] of rows) dl.append(el("dt", "", k), el("dd", "", v));
}

// ---- events tab ----

function eventsURL(d, offset) {
  return `/api/v1/namespaces/${encodeURIComponent(d.namespace)}/pods/${encodeURIComponent(d.name)}` +
    `/events?limit=${EVENTS_PAGE}&offset=${offset}`;
}

// pollEvents runs each tick while the Events tab is open. It always fetches
// page 1 with page 1's ETag, so a 304 costs nothing and re-renders nothing. On
// page 1 a 200 re-renders; on an older page it only raises the "new events" note.
async function pollEvents() {
  const d = state.drawer;
  const ev = d.ev;
  const before = ev.error;
  let changed = false;
  try {
    const r = await fetchJSON(eventsURL(d, 0), ev.page1 && ev.page1.etag);
    if (state.drawer !== d) return; // closed or another pod while in flight
    ev.error = null;
    if (r.status === 200) {
      changed = true;
      const hadPage1 = ev.page1 !== null;
      ev.page1 = { etag: r.etag, data: r.body };
      if (ev.page === 0) {
        ev.data = r.body;
        ev.newEvents = false;
      } else if (hadPage1) {
        ev.newEvents = true;
      }
    }
  } catch (e) {
    if (state.drawer !== d) return;
    ev.error = e.message || "network error";
  }
  if ((changed || ev.error !== before) && d.tab === "events") renderEvents();
}

// goToPage shows a page. Page 1 comes from the polled copy when there is one;
// an older page is fetched once and then left alone.
async function goToPage(page) {
  const d = state.drawer;
  const ev = d.ev;
  ev.page = page;
  if (page === 0) {
    ev.newEvents = false;
    if (ev.page1) ev.data = ev.page1.data;
    renderEvents();
    await pollEvents();
    return;
  }
  ev.loading = true;
  renderEvents();
  try {
    const r = await fetchJSON(eventsURL(d, page * EVENTS_PAGE), null);
    if (state.drawer !== d || ev.page !== page) return; // moved on meanwhile
    ev.loading = false;
    ev.error = null;
    ev.data = r.body;
    // The page emptied (events expired): go to the last page that has any.
    if (r.body.events.length === 0 && r.body.total > 0) {
      await goToPage(Math.ceil(r.body.total / EVENTS_PAGE) - 1);
      return;
    }
  } catch (e) {
    if (state.drawer !== d || ev.page !== page) return;
    ev.loading = false;
    ev.error = e.message || "network error";
  }
  renderEvents();
}

function renderEvents() {
  const d = state.drawer;
  const ev = d.ev;

  $("events-note").hidden = !(ev.newEvents && ev.page > 0);
  const banner = $("events-banner");
  banner.textContent = ev.error ? `Couldn't load events: ${ev.error}. Retrying.` : "";
  banner.hidden = !ev.error;

  const list = $("events-list");
  list.classList.toggle("stale", Boolean(ev.error || ev.loading));
  list.replaceChildren();
  const events = ev.data ? ev.data.events : [];
  for (const e of events) list.append(eventRow(ev, e));
  // Only messages that are actually clamped get to expand.
  for (const msg of list.querySelectorAll(".ev-msg[aria-expanded=false]")) {
    if (msg.scrollHeight <= msg.clientHeight + 1) {
      msg.disabled = true;
      msg.removeAttribute("aria-expanded");
      msg.removeAttribute("title");
    }
  }
  $("events-empty").hidden = !ev.data || ev.data.total > 0;
  renderPager();
}

function eventRow(ev, e) {
  const warn = e.type === "Warning";
  const li = el("li", "ev" + (warn ? " w" : ""));
  const top = el("div", "ev-top");
  top.append(el("span", warn ? "chip bad" : "chip", e.type || "Unknown"));
  top.append(el("b", "", e.reason || "-"));
  if (e.count > 1) top.append(el("span", "muted", `×${e.count}`));
  const seen = new Date(e.lastSeen);
  const age = el("span", "age", `${ago(seen)} ago`);
  age.dataset.ts = String(seen.getTime());
  age.title = seen.toLocaleString();
  top.append(age);
  li.append(top);

  const open = ev.expanded.has(e.name);
  const msg = el("button", "ev-msg", e.message);
  msg.type = "button";
  msg.setAttribute("aria-expanded", String(open));
  msg.title = "Click to expand";
  msg.addEventListener("click", () => {
    const now = !ev.expanded.has(e.name);
    if (now) ev.expanded.add(e.name);
    else ev.expanded.delete(e.name);
    msg.setAttribute("aria-expanded", String(now));
  });
  li.append(msg);
  return li;
}

// pageList returns the page numbers to show, with null for a gap, e.g.
// [0, null, 4, 5, 6, null, 9]. Short lists are shown whole.
function pageList(cur, pages) {
  if (pages <= 7) return [...Array(pages).keys()];
  const out = [0];
  const lo = Math.max(1, cur - 2);
  const hi = Math.min(pages - 2, cur + 2);
  if (lo > 1) out.push(null);
  for (let i = lo; i <= hi; i++) out.push(i);
  if (hi < pages - 2) out.push(null);
  out.push(pages - 1);
  return out;
}

function renderPager() {
  const d = state.drawer;
  const ev = d.ev;
  const pager = $("events-pager");
  const total = ev.data ? ev.data.total : 0;
  pager.hidden = d.tab !== "events" || total === 0;
  // Rebuilding drops focus; put it back on the current page if it was here.
  const hadFocus = pager.contains(document.activeElement);
  pager.replaceChildren();
  if (pager.hidden) return;

  const pages = Math.max(1, Math.ceil(total / EVENTS_PAGE));
  const first = ev.page * EVENTS_PAGE;
  const range = el("span", "muted range",
    `${Math.min(first + 1, total)}–${Math.min(first + EVENTS_PAGE, total)} of ${total}`);
  pager.append(range);

  const button = (text, label, page, disabled) => {
    const b = el("button", "", text);
    b.type = "button";
    b.setAttribute("aria-label", label);
    b.disabled = disabled;
    b.addEventListener("click", () => goToPage(page));
    return b;
  };
  pager.append(button("‹", "Previous page", ev.page - 1, ev.page === 0));
  let current = null;
  for (const i of pageList(ev.page, pages)) {
    if (i === null) {
      pager.append(el("span", "gap", "…"));
      continue;
    }
    const b = button(String(i + 1), `Page ${i + 1}`, i, false);
    if (i === ev.page) {
      b.setAttribute("aria-current", "page");
      current = b;
    }
    pager.append(b);
  }
  pager.append(button("›", "Next page", ev.page + 1, ev.page >= pages - 1));
  if (hadFocus && current) current.focus();
}

// refreshAges keeps "40s ago" honest between re-renders, which only happen when
// the data changes.
function refreshAges() {
  if (!state.drawer) return;
  for (const age of $("events-list").querySelectorAll("[data-ts]")) {
    age.textContent = `${ago(Number(age.dataset.ts))} ago`;
  }
}

async function pollDrawer() {
  const d = state.drawer;
  const poll = d && TABS[d.tab].poll;
  if (poll) await poll();
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
  closeDrawer(false);
  state.selected = name;
  state.pods = { etag: null, data: null, lastSeen: null, gone: false, error: null };
  renderNodes();
  renderPods();
  pollPods();
}

function closePods() {
  closeDrawer(false);
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
  await pollDrawer();
  running = false;
  // Hidden tabs stop polling; visibilitychange restarts it.
  if (!document.hidden) timer = setTimeout(tick, POLL_MS);
}

document.addEventListener("visibilitychange", () => {
  if (document.hidden) clearTimeout(timer);
  else tick();
});

$("pods-close").addEventListener("click", closePods);
$("drawer-close").addEventListener("click", () => closeDrawer(true));
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && state.drawer) {
    e.preventDefault();
    closeDrawer(true);
  }
});
for (const t of $("drawer-tabs").querySelectorAll("[role=tab]")) {
  t.addEventListener("click", () => setTab(t.dataset.tab));
}
// Arrow keys, Home and End move between tabs, as in the WAI-ARIA tabs pattern.
$("drawer-tabs").addEventListener("keydown", (e) => {
  if (!state.drawer) return;
  const i = TAB_ORDER.indexOf(state.drawer.tab);
  const next = { ArrowRight: i + 1, ArrowLeft: i - 1, Home: 0, End: TAB_ORDER.length - 1 }[e.key];
  if (next === undefined) return;
  e.preventDefault();
  const name = TAB_ORDER[(next + TAB_ORDER.length) % TAB_ORDER.length];
  setTab(name);
  $("tab-" + name).focus();
});
$("events-note-go").addEventListener("click", () => {
  if (!state.drawer) return;
  goToPage(0);
  // The note hides itself; keep focus in the drawer, on the page-1 button.
  const cur = $("events-pager").querySelector("[aria-current=page]");
  if (cur) cur.focus();
});
setInterval(() => {
  renderUpdated();
  refreshAges();
}, 1000);
tick();
