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
  const SYSTEM_NS = "kube-system";
  const PALETTE_SIZE = 6; // .cv-ns-0 … .cv-ns-5 in style.css
  const KI = 1024;
  const MI = KI * 1024;
  const GI = MI * 1024;

  const ps = {
    data: null,      // last /api/v1/pools body
    etag: null,
    lastOk: null,    // Date of the last 200 or 304
    error: null,     // message while the API is unreachable
    timer: null,
    running: false,
    off: new Set(),        // namespaces hidden with the legend
    colours: new Map(),    // namespace -> colour class, sticky for the session
    nextSlot: 0,
    selected: null,        // node whose pod table is open
  };

  const page = document.getElementById("pools-page");
  const onPools = () => location.hash === ROUTE;

  // ---- formatting (like kubectl describe) ----

  // cpu prints milli-CPU as describe does: "0", "310m", "1".
  function cpu(m) {
    if (m === 0) return "0";
    return m % 1000 === 0 ? String(m / 1000) : `${m}m`;
  }

  // mem uses the largest exact binary unit, as describe does for Ki/Mi/Gi
  // quantities ("332Mi", "1Gi", "6568224Ki"). Decimal quantities (e.g. "500M")
  // aren't exact, so they show as Mi with one decimal.
  function mem(b) {
    if (b === 0) return "0";
    for (const [unit, suffix] of [[GI, "Gi"], [MI, "Mi"], [KI, "Ki"]]) {
      if (b % unit === 0) return `${b / unit}${suffix}`;
    }
    return b >= MI ? `${(b / MI).toFixed(1)}Mi` : String(b);
  }

  // pct truncates like describe's percentOf: 310m of 3890m is 7.97%, shown as 7%.
  function pct(v, total) {
    return total ? Math.trunc(v / total * 100) : 0;
  }

  const RESOURCES = {
    cpu: { req: "cpuRequestMilli", lim: "cpuLimitMilli", alloc: "cpuMilli", fmt: cpu, label: "CPU" },
    mem: { req: "memRequestBytes", lim: "memLimitBytes", alloc: "memoryBytes", fmt: mem, label: "Memory" },
  };

  // ---- namespace colours ----

  // assignColours gives each namespace a colour the first time it's seen and
  // never changes it, so a poll or a legend filter never repaints the others.
  // kube-system is always neutral grey; namespaces past the palette share a
  // dark grey and are still named in the legend and on hover.
  function assignColours(namespaces) {
    for (const ns of namespaces) {
      if (ps.colours.has(ns)) continue;
      if (ns === SYSTEM_NS) ps.colours.set(ns, "cv-ns-sys");
      else if (ps.nextSlot < PALETTE_SIZE) ps.colours.set(ns, `cv-ns-${ps.nextSlot++}`);
      else ps.colours.set(ns, "cv-ns-other");
    }
  }

  const colourOf = (ns) => ps.colours.get(ns) || "cv-ns-other";

  // namespacesIn returns the sorted namespaces of every pod in the response.
  function namespacesIn(data) {
    const set = new Set();
    for (const pool of data.pools) {
      for (const n of pool.nodes) for (const p of n.pods) set.add(p.namespace);
    }
    return [...set].sort();
  }

  function sumPods(pods) {
    const t = { cpuRequestMilli: 0, cpuLimitMilli: 0, memRequestBytes: 0, memLimitBytes: 0 };
    for (const p of pods) for (const k in t) t[k] += p[k];
    return t;
  }

  // ---- rendering ----

  // render rebuilds the page. Focus is kept on the same control (by data-key),
  // since a poll can re-render while a legend chip or node is focused.
  function render() {
    const focused = document.activeElement && page.contains(document.activeElement)
      ? document.activeElement.dataset.key : undefined;
    draw();
    if (focused) {
      const again = [...page.querySelectorAll("[data-key]")].find((e) => e.dataset.key === focused);
      if (again) again.focus();
    }
  }

  function draw() {
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
    host.append(legend(namespacesIn(ps.data)));
    for (const pool of ps.data.pools) host.append(poolSection(pool));
  }

  function legend(namespaces) {
    const box = el("div", "cv-legend");
    box.append(el("span", "muted", "Namespaces (click to filter):"));
    if (namespaces.length === 0) {
      box.append(el("span", "muted", ps.data.filtered ? "No pods in the selected namespaces." : "No pods."));
      return box;
    }
    for (const ns of namespaces) {
      const off = ps.off.has(ns);
      const chip = el("button", "cv-chip" + (off ? " off" : ""));
      chip.type = "button";
      chip.dataset.key = "ns:" + ns;
      chip.setAttribute("aria-pressed", String(!off));
      chip.title = off ? `Show ${ns}` : `Hide ${ns}`;
      chip.append(el("i", "cv-dot " + colourOf(ns)), document.createTextNode(ns));
      chip.addEventListener("click", () => {
        if (ps.off.has(ns)) ps.off.delete(ns);
        else ps.off.add(ns);
        render();
      });
      box.append(chip);
    }
    if (namespaces.every((ns) => ps.off.has(ns))) {
      const reset = el("button", "cv-link", "All namespaces hidden. Show all");
      reset.type = "button";
      reset.dataset.key = "ns-reset";
      reset.addEventListener("click", () => {
        ps.off.clear();
        render();
      });
      box.append(reset);
    }
    return box;
  }

  const visiblePods = (n) => n.pods.filter((p) => !ps.off.has(p.namespace));

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
    // Pool percentages: summed requests over summed allocatable, truncated.
    const pods = pool.nodes.flatMap(visiblePods);
    const t = sumPods(pods);
    const alloc = { cpuMilli: 0, memoryBytes: 0 };
    for (const n of pool.nodes) {
      alloc.cpuMilli += n.allocatable.cpuMilli;
      alloc.memoryBytes += n.allocatable.memoryBytes;
    }
    facts.push(`${pods.length} pod${pods.length === 1 ? "" : "s"}`);
    facts.push(`CPU req ${pct(t.cpuRequestMilli, alloc.cpuMilli)}%`);
    facts.push(`Mem req ${pct(t.memRequestBytes, alloc.memoryBytes)}%`);
    head.append(el("span", "muted", facts.join(" · ")));
    sec.append(head);

    const grid = el("div", "cv-grid");
    const hdr = el("div", "cv-row cv-row-head");
    hdr.append(el("span", "", "Node"), el("span", "", "CPU requests by pod (of allocatable)"),
      el("span", "", "Memory requests by pod (of allocatable)"), el("span", "num", "Pods"));
    grid.append(hdr);
    for (const n of pool.nodes) grid.append(nodeRow(n));
    sec.append(grid);
    const open = pool.nodes.find((n) => n.name === ps.selected);
    if (open) sec.append(podTable(open));
    return sec;
  }

  function select(name) {
    ps.selected = ps.selected === name ? null : name;
    render();
  }

  function nodeRow(n) {
    const pods = visiblePods(n);
    const shown = pods.length !== n.pods.length;
    const totals = shown ? sumPods(pods) : n.totals;

    const selected = n.name === ps.selected;
    const row = el("div", "cv-row cv-node" + (selected ? " sel" : ""));
    // The whole row is clickable; the name is a real button for the keyboard
    // (its click bubbles to the row, so there's one handler).
    row.addEventListener("click", () => select(n.name));
    const name = el("span", "cv-name");
    const open = el("button", "cv-link mono", n.name);
    open.type = "button";
    open.dataset.key = "node:" + n.name;
    open.setAttribute("aria-expanded", String(selected));
    open.title = selected ? "Hide pods" : "Show pods";
    name.append(open);
    if (n.status !== "Ready") name.append(el("span", "badge " + n.status, n.status));
    if (!n.schedulable) name.append(el("span", "badge cordoned", "Cordoned"));
    row.append(name);
    row.append(resourceCell(n, pods, totals, RESOURCES.cpu, shown));
    row.append(resourceCell(n, pods, totals, RESOURCES.mem, shown));
    row.append(el("span", "num", String(pods.length)));
    return row;
  }

  // resourceCell is one stacked bar (a segment per pod with a request, coloured by
  // namespace) and the describe-style totals under it.
  function resourceCell(n, pods, totals, r, shown) {
    const alloc = n.allocatable[r.alloc];
    const cell = el("span", "cv-cell");
    const bar = el("span", "cv-stack");
    bar.setAttribute("role", "img");
    bar.setAttribute("aria-label", `${r.label} requests by pod on ${n.name}`);
    if (alloc > 0) {
      for (const p of pods) {
        const v = p[r.req];
        if (v <= 0) continue; // BestEffort and zero requests take no space
        const seg = el("span", "cv-seg " + colourOf(p.namespace));
        // CSSOM, not a style attribute: CSP allows it. The exact share, not the
        // truncated %, so the segments add up.
        seg.style.width = `${v / alloc * 100}%`;
        seg.title = `${p.namespace}/${p.name}: ${r.fmt(v)} (${pct(v, alloc)}%)`;
        bar.append(seg);
      }
    }
    cell.append(bar);

    const line = el("span", "cv-totals");
    if (alloc > 0) {
      const req = totals[r.req];
      const lim = totals[r.lim];
      const limPct = pct(lim, alloc);
      let text = `${r.fmt(req)} / ${r.fmt(alloc)} · req ${pct(req, alloc)}% · lim ${limPct}%`;
      if (shown) text += " (shown)";
      if (ps.data.filtered) text += " (filtered namespaces)";
      line.append(document.createTextNode(text));
      if (limPct > 100) line.append(el("span", "cv-over", " ⚠ overcommitted"));
    } else {
      line.append(document.createTextNode("allocatable unknown"));
    }
    const none = pods.filter((p) => p[r.req] === 0).length;
    if (none > 0) line.append(el("span", "muted", ` · +${none} with no requests`));
    cell.append(line);
    return cell;
  }

  // podTable is the kubectl describe node "Non-terminated Pods" table, with each
  // value's share of allocatable. Rows follow the legend filter.
  function podTable(n) {
    const pods = visiblePods(n);
    const box = el("div", "cv-detail");
    const head = el("div", "cv-detail-head");
    head.append(el("h3", "mono", n.name));
    head.append(el("span", "muted", `Non-terminated pods (${pods.length} shown)`));
    const close = el("button", "cv-link", "Close");
    close.type = "button";
    close.dataset.key = "close:" + n.name;
    close.addEventListener("click", () => select(n.name));
    head.append(close);
    box.append(head);
    if (pods.length === 0) {
      box.append(el("p", "muted", "No pods to show on this node."));
      return box;
    }

    const table = el("table", "cv-table");
    const tr = el("tr");
    for (const h of ["Namespace", "Pod", "QoS", "CPU req", "CPU lim", "Mem req", "Mem lim"]) {
      tr.append(el("th", h.includes(" ") ? "num" : "", h));
    }
    const thead = el("thead");
    thead.append(tr);
    const tbody = el("tbody");
    for (const p of pods) {
      const row = el("tr");
      const ns = el("td");
      ns.append(el("i", "cv-dot " + colourOf(p.namespace)), document.createTextNode(p.namespace));
      const qos = el("td");
      qos.append(el("span", "chip cv-q-" + (p.qos || "none"), p.qos || "-"));
      row.append(ns, el("td", "mono", p.name), qos);
      for (const [r, key] of [[RESOURCES.cpu, "req"], [RESOURCES.cpu, "lim"], [RESOURCES.mem, "req"], [RESOURCES.mem, "lim"]]) {
        const v = p[r[key]];
        const alloc = n.allocatable[r.alloc];
        const td = el("td", "num", `${r.fmt(v)} (${pct(v, alloc)}%)`);
        if (key === "lim" && v === 0) td.title = "no limit"; // 0 means unset, as in describe
        row.append(td);
      }
      tbody.append(row);
    }
    table.append(thead, tbody);
    const scroll = el("div", "cv-scroll");
    scroll.append(table);
    box.append(scroll);
    return box;
  }

  // ---- data ----

  // poll re-renders only when something changed: a 200, or the API going away or
  // coming back. A 304 costs no DOM work.
  async function poll() {
    const before = ps.error;
    let changed = false;
    try {
      const r = await fetchJSON("/api/v1/pools", ps.etag);
      ps.lastOk = new Date();
      ps.error = null;
      if (r.status === 200) {
        ps.data = r.body;
        ps.etag = r.etag;
        assignColours(namespacesIn(r.body));
        // The selected node left the cluster: close its table.
        if (ps.selected && !r.body.pools.some((pl) => pl.nodes.some((n) => n.name === ps.selected))) {
          ps.selected = null;
        }
        changed = true;
      }
    } catch (e) {
      ps.error = e.message || "network error";
    }
    if (onPools() && (changed || ps.error !== before)) render();
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
