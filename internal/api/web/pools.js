// Node pool capacity map, the #/pools page. Plain DOM, no build step.
//
// It runs after app.js (both are deferred) and reuses its globals: el, fetchJSON,
// cores-style helpers, POLL_MS, tick and closeDrawer. Everything of its own lives
// in this IIFE so nothing collides with app.js's top-level names.
//
// Routing: the hash picks the page. On #/pools the body gets a route-pools class
// (CSS hides the Nodes page, so its DOM and ETags survive), the pod drawer is
// closed through its own close path, and this file polls /api/v1/pools. app.js's
// tick() returns early on #/pools, so the Nodes polling pauses; coming back calls
// tick() for an immediate refresh.
//
// CSP (default-src 'self') blocks inline style attributes, so nothing here writes
// a style attribute or cssText. Segment widths go through the CSSOM (element.style.width),
// which CSP allows, and colours are CSS classes.
"use strict";

(() => {
  const ROUTE = "#/pools";

  const ps = {
    data: null,      // last /api/v1/pools body
    etag: null,
    lastOk: null,    // Date of the last 200 or 304
    error: null,     // message while the API is unreachable
    timer: null,
    running: false,
  };

  const page = document.getElementById("pools-page");
  const onPools = () => location.hash === ROUTE;

  // ---- rendering ----

  function render() {
    page.replaceChildren(toolbar());
    const banner = el("div", "banner", "");
    banner.setAttribute("role", "status");
    if (ps.error) {
      banner.textContent = ps.lastOk
        ? `Can't reach the API (${ps.error}). Showing data from ${timeOf(ps.lastOk)}, retrying.`
        : `Can't reach the API (${ps.error}), retrying.`;
    }
    banner.hidden = !ps.error;
    page.append(banner);

    const host = el("div", "cv-host" + (ps.error ? " stale" : ""));
    page.append(host);
    if (!ps.data) {
      host.append(el("p", "muted", "Loading…"));
      return;
    }
    if (ps.data.pools.length === 0) {
      host.append(el("p", "muted", "No nodes."));
      return;
    }
    for (const pool of ps.data.pools) host.append(poolSection(pool));
  }

  function toolbar() {
    const bar = el("div", "cv-toolbar");
    bar.append(el("span", "updated", updatedText()));
    return bar;
  }

  function updatedText() {
    if (!ps.lastOk) return "";
    return `Updated ${Math.round((Date.now() - ps.lastOk) / 1000)}s ago`;
  }

  function poolSection(pool) {
    const sec = el("section", "cv-pool");
    const head = el("div", "cv-pool-head");
    head.append(el("h2", "", pool.name));
    const facts = [];
    if (pool.instanceType) facts.push(pool.instanceType);
    facts.push(`${pool.nodeCount} node${pool.nodeCount === 1 ? "" : "s"}`);
    head.append(el("span", "muted", facts.join(" · ")));
    sec.append(head);
    const list = el("ul", "cv-plain");
    for (const n of pool.nodes) list.append(el("li", "mono", n.name));
    sec.append(list);
    return sec;
  }

  // ---- data ----

  async function poll() {
    try {
      const r = await fetchJSON("/api/v1/pools", ps.etag);
      ps.lastOk = new Date();
      ps.error = null;
      if (r.status === 200) {
        ps.data = r.body;
        ps.etag = r.etag;
      }
    } catch (e) {
      ps.error = e.message || "network error";
    }
    if (onPools()) render();
  }

  // loop mirrors app.js's tick: the next poll is scheduled only after this one
  // finishes, and only while #/pools is shown in a visible tab.
  async function loop() {
    if (ps.running || !onPools()) return;
    ps.running = true;
    clearTimeout(ps.timer);
    await poll();
    ps.running = false;
    if (!document.hidden && onPools()) ps.timer = setTimeout(loop, POLL_MS);
  }

  // ---- routing ----

  function route() {
    const on = onPools();
    document.body.classList.toggle("route-pools", on);
    for (const [id, active] of [["nav-nodes", !on], ["nav-pools", on]]) {
      const a = document.getElementById(id);
      if (active) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    }
    if (on) {
      closeDrawer(false); // the drawer's own path: clears its state, so its polling stops
      render();
      loop();
    } else {
      clearTimeout(ps.timer);
      tick();
    }
  }

  window.addEventListener("hashchange", route);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) clearTimeout(ps.timer);
    else loop();
  });
  setInterval(() => {
    if (!onPools()) return;
    const u = page.querySelector(".cv-toolbar .updated");
    if (u) u.textContent = updatedText();
  }, 1000);
  route();
})();
