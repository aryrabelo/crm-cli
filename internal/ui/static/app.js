// crm ui: a spreadsheet over the crm JSON API. No framework, no build step:
// Tabulator (vendored) draws the grid; everything else is plain DOM.
// Cell values are always rendered as text (textContent), never as HTML.
"use strict";

(function () {
  const TOKEN_KEY = "crm-ui-token";
  const TABLE_KEY = "crm-ui-table";
  const ACTIVE_POLL_MS = 1000;
  const IDLE_POLL_MS = 2000;

  const $ = (id) => document.getElementById(id);
  const state = {
    table: "",
    only: [],
    view: null,
    grid: null,
    signature: "",
    wasActive: false,
    lastRun: "",
    timer: 0,
    polling: false,
    runColumn: "",
    importText: "",
  };

  // ---- token ---------------------------------------------------------------

  // The URL printed by crm ui carries the token in its fragment (never sent
  // to the server); keep it for this tab only and drop it from the address bar.
  function takeTokenFromURL() {
    const m = /(?:^|[#&])token=([^&]+)/.exec(location.hash);
    if (m) {
      sessionStorage.setItem(TOKEN_KEY, decodeURIComponent(m[1]));
      history.replaceState(null, "", location.pathname + location.search);
    }
  }

  function token() {
    return sessionStorage.getItem(TOKEN_KEY) || "";
  }

  function askToken() {
    const d = $("token-dialog");
    if (!d.open) d.showModal();
  }

  // ---- API -----------------------------------------------------------------

  class APIError extends Error {
    constructor(status, message) {
      super(message);
      this.status = status;
    }
  }

  async function api(path, opts = {}) {
    const headers = { Authorization: "Bearer " + token() };
    let body;
    if (opts.json !== undefined) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(opts.json);
    } else if (opts.csv !== undefined) {
      headers["Content-Type"] = "text/csv";
      body = opts.csv;
    }
    const res = await fetch(path, { method: opts.method || "GET", headers, body, credentials: "omit", cache: "no-store" });
    if (res.status === 401) {
      sessionStorage.removeItem(TOKEN_KEY);
      askToken();
      throw new APIError(401, "unauthorized: open the URL printed by crm ui");
    }
    if (!res.ok) {
      let msg = res.status + " " + res.statusText;
      try {
        const j = await res.json();
        if (j && j.error) msg = j.error;
      } catch (_) { /* not JSON */ }
      throw new APIError(res.status, msg);
    }
    if (opts.raw) return res;
    return res.json();
  }

  const enc = encodeURIComponent;
  const tablePath = (suffix = "") => "/api/tables/" + enc(state.table) + suffix;
  const onlyQuery = () => state.only.map((o) => "only=" + enc(o)).join("&");

  // ---- small DOM helpers -----------------------------------------------------

  function el(tag, attrs, ...children) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else n.setAttribute(k, v);
    }
    for (const c of children) {
      if (c === null || c === undefined) continue;
      n.append(c instanceof Node ? c : String(c));
    }
    return n;
  }

  let toastTimer = 0;
  function toast(msg, isError) {
    const t = $("toast");
    t.textContent = msg;
    t.className = "toast" + (isError ? " error" : "");
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 3000);
  }

  function showError(id, err) {
    const n = $(id);
    n.textContent = err ? (err.message || String(err)) : "";
    n.hidden = !err;
  }

  function valueText(v) {
    if (v === null || v === undefined) return "";
    if (typeof v === "string") return v;
    return JSON.stringify(v);
  }

  // ---- grid ------------------------------------------------------------------

  const BADGES = {
    none: ["▶", "not run yet"],
    pending: ["◷", "pending: the run stopped before this cell (resume it with crm run --resume)"],
    running: ["", "running: queued or being computed"],
    ok: ["✓", "ok"],
    empty: ["—", "empty: no input, or no provider had an answer"],
    error: ["!", "error"],
    uncertain: ["?", "uncertain cost"],
    skipped: ["$", "skipped: budget exhausted"],
  };

  function titleFormatter(col) {
    return () => {
      const t = el("span", { class: "col-title" }, el("span", { text: col.name }));
      if (col.type !== "input") t.append(el("span", { class: "col-type " + col.type, text: col.type }));
      return t;
    };
  }

  function plainFormatter(cell) {
    return el("span", { class: "v", text: valueText(cell.getValue()) });
  }

  function computedFormatter(i) {
    return (cell) => {
      const s = cell.getRow().getData()["s" + i];
      const status = s ? s.status : "none";
      const [glyph, label] = BADGES[status] || BADGES.none;
      const wrap = el("span", { class: "cell " + status });
      const badge = el("span", { class: "badge " + status, text: glyph });
      let tip = label;
      if (s) {
        if (s.error) tip += ": " + s.error;
        if (s.provider) tip += "\nprovider: " + s.provider + (s.cache_hit ? " (cache)" : "");
        if (s.cost && !/^\$0(\.0*)?$/.test(s.cost)) tip += "\ncost: " + s.cost;
        tip += "\nrun " + s.run_id;
      }
      wrap.title = tip;
      const text = valueText(cell.getValue());
      // A failed or skipped cell has no value: show why (the full message is
      // in the tooltip; errors arrive redacted).
      if (!text && s && s.error && (status === "error" || status === "skipped" || status === "uncertain")) {
        wrap.append(badge, el("span", { class: "v why", text: s.error }));
      } else {
        wrap.append(badge, el("span", { class: "v", text }));
      }
      return wrap;
    };
  }

  function columnDefs(view) {
    const defs = [{
      title: "#", field: "_row", width: 52, hozAlign: "right", headerSort: true, frozen: true,
      formatter: (cell) => el("span", { class: "rownum", text: String(cell.getValue()) }),
    }];
    view.columns.forEach((col, i) => {
      const menu = [];
      if (col.runnable) {
        menu.push({ label: "▶ Run column…", action: () => openRunDialog(col.name) });
      }
      // Labels as elements: Tabulator puts string labels in innerHTML.
      menu.push({ label: () => el("span", { text: "Only rows where " + col.name + " is filled" }), action: () => addFilter(col.name + "!=") });
      defs.push({
        title: col.name,
        field: "c" + i,
        titleFormatter: titleFormatter(col),
        // An element, not a string: Tabulator puts tooltip strings in innerHTML.
        headerTooltip: () => el("div", { text: col.definition ? col.type + ": " + col.definition : col.type }),
        headerMenu: menu,
        headerFilter: "input",
        headerFilterPlaceholder: "filter…",
        headerFilterFunc: (term, value) => valueText(value).toLowerCase().includes(String(term).toLowerCase()),
        formatter: col.runnable ? computedFormatter(i) : plainFormatter,
        sorter: (a, b) => valueText(a).localeCompare(valueText(b), undefined, { numeric: true }),
        maxInitialWidth: 320,
      });
    });
    return defs;
  }

  function rowsData(view) {
    return view.rows.map((r) => {
      const d = { id: r.id, _row: r.row };
      r.values.forEach((v, i) => { d["c" + i] = v; });
      r.states.forEach((s, i) => { d["s" + i] = s; });
      return d;
    });
  }

  function renderView(view) {
    state.view = view;
    const signature = JSON.stringify(view.columns.map((c) => [c.name, c.type, c.definition]));
    const data = rowsData(view);
    $("row-count").textContent = view.rows.length + (view.rows.length === 1 ? " row" : " rows") +
      (state.only.length ? " (filtered)" : "");
    if (!state.grid) {
      state.grid = new Tabulator("#grid", {
        data,
        columns: columnDefs(view),
        index: "id",
        layout: "fitDataStretch",
        height: "100%",
        placeholder: "No rows",
        headerFilterLiveFilterDelay: 150,
        columnDefaults: { resizable: true, headerHozAlign: "left" },
      });
      state.signature = signature;
      return;
    }
    if (signature !== state.signature) {
      state.grid.setColumns(columnDefs(view));
      state.signature = signature;
    }
    state.grid.replaceData(data);
  }

  function resetGrid() {
    if (state.grid) state.grid.destroy();
    state.grid = null;
    state.signature = "";
  }

  // ---- tables ----------------------------------------------------------------

  async function loadTables(select) {
    const { tables } = await api("/api/tables");
    const sel = $("table-select");
    sel.replaceChildren(...tables.map((t) => el("option", { value: t.name, text: t.name + " · " + t.rows })));
    $("empty").hidden = tables.length > 0;
    const names = tables.map((t) => t.name);
    const want = select || state.table || localStorage.getItem(TABLE_KEY) || "";
    const next = names.includes(want) ? want : (names[0] || "");
    sel.value = next;
    if (next !== state.table) {
      state.table = next;
      state.only = [];
      $("filter").value = "";
      resetGrid();
    }
    if (state.table) localStorage.setItem(TABLE_KEY, state.table);
  }

  async function loadView() {
    if (!state.table) return;
    const q = onlyQuery();
    renderView(await api(tablePath(q ? "?" + q : "")));
  }

  function addFilter(expr) {
    const parts = $("filter").value.split(";").map((s) => s.trim()).filter(Boolean);
    if (!parts.includes(expr)) parts.push(expr);
    $("filter").value = parts.join("; ");
    applyFilter();
  }

  async function applyFilter() {
    state.only = $("filter").value.split(";").map((s) => s.trim()).filter(Boolean);
    try {
      await loadView();
    } catch (e) {
      toast(e.message, true);
    }
  }

  // ---- run summary and polling -----------------------------------------------

  function money(usd) {
    return "$" + Number(usd || 0).toFixed(4).replace(/0+$/, "").replace(/\.$/, "");
  }

  function renderRun(r) {
    const box = $("run-summary");
    if (!r) {
      box.hidden = true;
      return;
    }
    box.hidden = false;
    const done = r.items - r.pending;
    const pct = r.items ? Math.round((100 * done) / r.items) : 100;
    const status = r.active ? "running" : r.status.replace("_", " ");
    box.classList.toggle("done", !r.active);
    $("run-title").textContent = "Run " + r.id + " · " + r.columns + " · " + status;
    const parts = [done + "/" + r.items + " done", "ok " + r.ok, "empty " + r.empty, "failed " + r.failed];
    if (r.skipped) parts.push("skipped " + r.skipped);
    parts.push("cache hits " + r.cache_hits, "calls " + r.calls);
    if (r.tokens_in || r.tokens_out) parts.push("tokens " + r.tokens_in + " in / " + r.tokens_out + " out");
    if (!r.active) parts.push(r.duration);
    $("run-counts").textContent = parts.join(" · ");
    let m = "spent " + money(r.spent_usd) + " of " + money(r.budget_usd) + " budget";
    if (r.uncertain_items) m += " (" + money(r.uncertain_spent_usd) + " uncertain)";
    $("run-money").textContent = m;
    $("run-progress").style.width = pct + "%";
    $("run-cancel").hidden = !(r.active && r.cancellable);
    $("run-cancel").dataset.run = r.id;
    showError("run-error", r.error ? new Error(r.error) : null);
  }

  function schedule(ms) {
    clearTimeout(state.timer);
    state.timer = setTimeout(poll, ms);
  }

  // poll follows the table's runs: every second while one executes (started
  // here or by crm run in a terminal), refreshing the grid; every two seconds
  // otherwise, to notice a run started elsewhere. A run that started and
  // finished between two polls still refreshes the grid once.
  async function poll() {
    if (state.polling) return;
    state.polling = true;
    let active = false;
    try {
      if (state.table && token()) {
        const runs = await api(tablePath("/runs"));
        const r = (runs.live_run && runs.live_run.active) ? runs.live_run : runs.latest_run;
        active = !!(r && r.active);
        renderRun(r);
        const seen = r ? [state.table, r.id, r.status, r.finished_at].join(":") : state.table;
        if (active || state.wasActive || seen !== state.lastRun) await loadView();
        state.wasActive = active;
        state.lastRun = seen;
      }
    } catch (e) {
      if (e.status !== 401) toast(e.message, true);
    } finally {
      state.polling = false;
      schedule(active ? ACTIVE_POLL_MS : IDLE_POLL_MS);
    }
  }

  // ---- run dialog --------------------------------------------------------------

  function estimateTable(est) {
    const head = el("tr", {}, ...["Column", "Items", "Cache hits", "No input", "Calls", "Est. cost", "Worst case"].map((h) => el("th", { text: h })));
    const row = (ce, cls) => el("tr", cls ? { class: cls } : {},
      el("td", { text: ce.column }), el("td", { text: String(ce.items) }), el("td", { text: String(ce.cache_hits) }),
      el("td", { text: String(ce.no_input) }), el("td", { text: String(ce.calls) }), el("td", { text: ce.expected }),
      el("td", { text: ce.worst }));
    const t = el("table", {}, head, ...est.columns.map((c) => row(c)));
    if (est.columns.length > 1) t.append(row(est.total, "total"));
    const box = el("div", {}, t);
    if (est.unbounded) {
      box.append(el("div", { class: "note", text: est.total.unbounded_items +
        " item(s) send a prompt built from a column computed earlier in this run: their price is not known yet (unbounded). The budget still caps the run; items that do not fit are skipped." }));
    }
    return box;
  }

  async function openRunDialog(name) {
    state.runColumn = name;
    $("run-dialog-title").textContent = "Run column " + name;
    $("run-scope").textContent = state.only.length ? "Rows matching: " + state.only.join("; ") : "All rows of " + state.table;
    $("run-estimate").replaceChildren(el("span", { class: "muted", text: "Estimating…" }));
    $("run-budget").value = "";
    $("run-budget-hint").textContent = "";
    showError("run-error-msg", null);
    $("run-confirm").disabled = true;
    $("run-dialog").showModal();
    try {
      const est = await api(tablePath("/estimate"), { method: "POST", json: { columns: [name], only: state.only } });
      $("run-estimate").replaceChildren(estimateTable(est));
      const worst = est.total.worst_usd;
      if (est.needs_budget) {
        $("run-budget").value = worst > 0 ? String(Number(worst.toFixed(6))) : "";
        $("run-budget").required = true;
        $("run-budget-hint").textContent = "Required: this run may spend money. Prefilled with the worst case; items that do not fit are skipped (resumable).";
      } else {
        $("run-budget").required = false;
        $("run-budget-hint").textContent = "Free: every item is a cache hit, has no input, or uses free providers.";
      }
      $("run-confirm").disabled = false;
    } catch (e) {
      $("run-estimate").replaceChildren();
      showError("run-error-msg", e);
    }
  }

  async function confirmRun(ev) {
    ev.preventDefault();
    const budget = $("run-budget").value.trim();
    $("run-confirm").disabled = true;
    try {
      const res = await api(tablePath("/runs"), { method: "POST", json: { columns: [state.runColumn], only: state.only, budget } });
      $("run-dialog").close();
      renderRun(res.run);
      state.wasActive = true;
      await loadView();
      schedule(0);
    } catch (e) {
      showError("run-error-msg", e);
      $("run-confirm").disabled = false;
    }
  }

  async function cancelRun() {
    const id = $("run-cancel").dataset.run;
    try {
      await api("/api/runs/" + enc(id) + "/cancel", { method: "POST", json: {} });
      toast("Cancelling run " + id + "…");
      schedule(0);
    } catch (e) {
      toast(e.message, true);
    }
  }

  // ---- add column dialog -------------------------------------------------------

  function columnSpec() {
    const type = $("col-type").value;
    const spec = { name: $("col-name").value.trim(), type };
    const list = (s) => s.split(",").map((x) => x.trim()).filter(Boolean);
    if (type === "enrich" || type === "ai") {
      const wf = list($("col-waterfall").value);
      if (wf.length) spec.waterfall = wf;
    }
    if (type === "enrich") {
      if ($("col-from").value) spec.from = $("col-from").value;
      if ($("col-field").value.trim()) spec.field = $("col-field").value.trim();
    }
    if (type === "formula") spec.expr = $("col-expr").value;
    if (type === "ai") {
      spec.prompt = $("col-prompt").value;
      if ($("col-output").value.trim()) spec.output = $("col-output").value.trim();
      if ($("col-explain").checked) spec.explain = true;
    }
    return spec;
  }

  function syncColumnType() {
    const type = $("col-type").value;
    for (const n of $("column-form").querySelectorAll("[data-for]")) {
      n.hidden = !n.dataset.for.split(" ").includes(type);
    }
    $("col-estimate").hidden = true;
  }

  function openColumnDialog() {
    if (!state.table) {
      toast("Import a CSV first", true);
      return;
    }
    $("column-form").reset();
    const cols = state.view ? state.view.columns.map((c) => c.name) : [];
    const key = state.view && state.view.key_column;
    $("col-from").replaceChildren(...cols.map((c) => el("option", { value: c, text: c + (c === key ? " (key)" : "") })));
    if (key) $("col-from").value = key;
    showError("col-error", null);
    syncColumnType();
    $("column-dialog").showModal();
  }

  async function estimateColumn() {
    showError("col-error", null);
    const box = $("col-estimate");
    box.hidden = false;
    box.replaceChildren(el("span", { class: "muted", text: "Estimating…" }));
    try {
      const est = await api(tablePath("/estimate"), { method: "POST", json: { preview: columnSpec(), only: state.only } });
      box.replaceChildren(estimateTable(est));
    } catch (e) {
      box.hidden = true;
      showError("col-error", e);
    }
  }

  async function addColumn(ev) {
    ev.preventDefault();
    showError("col-error", null);
    try {
      const spec = columnSpec();
      await api(tablePath("/columns"), { method: "POST", json: spec });
      $("column-dialog").close();
      toast("Column " + spec.name + " added" + (spec.type === "enrich" || spec.type === "ai" ? ": run it from its header menu" : ""));
      await loadTables();
      await loadView();
    } catch (e) {
      showError("col-error", e);
    }
  }

  // ---- import / export -----------------------------------------------------------

  // csvHeader splits the first record of a CSV (quotes honored) for the key
  // picker; the server does the real parsing.
  function csvHeader(text) {
    const out = [];
    let cur = "";
    let quoted = false;
    for (let i = text.charCodeAt(0) === 0xfeff ? 1 : 0; i < text.length; i++) {
      const ch = text[i];
      if (quoted) {
        if (ch === '"' && text[i + 1] === '"') { cur += '"'; i++; } else if (ch === '"') quoted = false; else cur += ch;
      } else if (ch === '"') quoted = true;
      else if (ch === ",") { out.push(cur); cur = ""; }
      else if (ch === "\n" || ch === "\r") break;
      else cur += ch;
    }
    out.push(cur);
    return out.filter((h) => h !== "");
  }

  async function chooseImport(file) {
    if (!file) return;
    state.importText = await file.text();
    $("import-file-name").textContent = file.name + " · " + Math.max(0, state.importText.split(/\r?\n/).filter(Boolean).length - 1) + " rows";
    const base = file.name.replace(/\.[^.]*$/, "").replace(/[^A-Za-z0-9_.-]+/g, "-").replace(/^[^A-Za-z0-9]+/, "");
    $("import-as").value = base || "table";
    $("import-key").replaceChildren(el("option", { value: "", text: "(none)" }),
      ...csvHeader(state.importText).map((h) => el("option", { value: h, text: h })));
    showError("import-error", null);
    $("import-dialog").showModal();
  }

  async function importCSV(ev) {
    ev.preventDefault();
    const as = $("import-as").value.trim();
    const key = $("import-key").value;
    try {
      const q = "as=" + enc(as) + (key ? "&key=" + enc(key) : "");
      const res = await api("/api/tables/import?" + q, { method: "POST", csv: state.importText });
      $("import-dialog").close();
      state.importText = "";
      toast("Imported " + res.rows + " rows into " + res.table + " (" + res.created + " created, " + res.updated + " updated)");
      await loadTables(res.table);
      await loadView();
      schedule(0);
    } catch (e) {
      showError("import-error", e);
    }
  }

  async function exportCSV() {
    if (!state.table) return;
    try {
      const q = onlyQuery();
      const res = await api(tablePath("/export" + (q ? "?" + q : "")), { raw: true });
      const url = URL.createObjectURL(await res.blob());
      const a = el("a", { href: url, download: state.table + ".csv" });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      toast(e.message, true);
    }
  }

  function wireDrop() {
    let depth = 0;
    const show = (on) => { $("drop").hidden = !on; };
    const hasFiles = (e) => e.dataTransfer && Array.from(e.dataTransfer.types).includes("Files");
    window.addEventListener("dragenter", (e) => { if (hasFiles(e)) { depth++; show(true); } });
    window.addEventListener("dragleave", () => { depth = Math.max(0, depth - 1); if (!depth) show(false); });
    window.addEventListener("dragover", (e) => { if (hasFiles(e)) e.preventDefault(); });
    window.addEventListener("drop", (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      depth = 0;
      show(false);
      chooseImport(e.dataTransfer.files[0]);
    });
  }

  // ---- start -------------------------------------------------------------------

  async function start() {
    try {
      await loadTables();
      await loadView();
    } catch (e) {
      if (e.status !== 401) toast(e.message, true);
    }
    schedule(0);
  }

  function wire() {
    for (const b of document.querySelectorAll("dialog [data-close]")) {
      b.addEventListener("click", () => b.closest("dialog").close());
    }
    $("token-form").addEventListener("submit", () => {
      sessionStorage.setItem(TOKEN_KEY, $("token-input").value.trim());
      $("token-input").value = "";
      start();
    });
    $("table-select").addEventListener("change", async () => {
      state.table = $("table-select").value;
      localStorage.setItem(TABLE_KEY, state.table);
      state.only = [];
      $("filter").value = "";
      state.wasActive = false;
      resetGrid();
      try {
        await loadView();
      } catch (e) {
        toast(e.message, true);
      }
      schedule(0);
    });
    $("filter-form").addEventListener("submit", (e) => { e.preventDefault(); applyFilter(); });
    $("add-column").addEventListener("click", openColumnDialog);
    $("col-type").addEventListener("change", syncColumnType);
    $("col-estimate-btn").addEventListener("click", estimateColumn);
    $("column-form").addEventListener("submit", addColumn);
    $("run-form").addEventListener("submit", confirmRun);
    $("run-cancel").addEventListener("click", cancelRun);
    $("import").addEventListener("click", () => $("import-file").click());
    $("import-file").addEventListener("change", (e) => { chooseImport(e.target.files[0]); e.target.value = ""; });
    $("import-form").addEventListener("submit", importCSV);
    $("export").addEventListener("click", exportCSV);
    wireDrop();
  }

  takeTokenFromURL();
  wire();
  if (!token()) askToken();
  else start();
})();
