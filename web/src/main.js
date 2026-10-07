// Fleetling's browser code: HTMX plus a few small helpers. Bundled by
// esbuild into web/static/dist/app.js. No inline scripts anywhere, so the
// CSP can stay at script-src 'self'.
import htmx from "htmx.org";
import { setupLogs } from "./logs.js";

window.htmx = htmx;

// Every HTMX request carries the CSRF token from the page's meta tag.
document.addEventListener("htmx:configRequest", (e) => {
  const meta = document.querySelector('meta[name="csrf-token"]');
  if (meta && meta.content) e.detail.headers["X-CSRF-Token"] = meta.content;
});

// Theme: follows the system until toggled, then remembers the choice.
const THEME_KEY = "fleetling-theme";
function applyTheme(t) {
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
}
try { applyTheme(localStorage.getItem(THEME_KEY)); } catch { /* storage blocked */ }

document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-theme-toggle]");
  if (!btn) return;
  const dark = document.documentElement.dataset.theme
    ? document.documentElement.dataset.theme === "dark"
    : matchMedia("(prefers-color-scheme: dark)").matches;
  const next = dark ? "light" : "dark";
  applyTheme(next);
  try { localStorage.setItem(THEME_KEY, next); } catch { /* storage blocked */ }
});

// Forms with data-confirm ask first.
document.addEventListener("submit", (e) => {
  const msg = e.target.dataset && e.target.dataset.confirm;
  if (msg && !window.confirm(msg)) e.preventDefault();
});

// Sortable columns: click a header with data-sort="text" or "num".
document.addEventListener("click", (e) => {
  const th = e.target.closest("th[data-sort]");
  if (!th) return;
  const table = th.closest("table");
  const idx = [...th.parentNode.children].indexOf(th);
  const asc = th.getAttribute("aria-sort") !== "ascending";
  table.querySelectorAll("th").forEach((h) => h.removeAttribute("aria-sort"));
  th.setAttribute("aria-sort", asc ? "ascending" : "descending");
  const num = th.dataset.sort === "num";
  const key = (row) => {
    const cell = row.children[idx];
    if (!cell) return "";
    const v = cell.dataset.value ?? cell.textContent.trim();
    return num ? parseFloat(v) || 0 : v.toLowerCase();
  };
  const body = table.tBodies[0];
  [...body.rows]
    .sort((a, b) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0) * (asc ? 1 : -1))
    .forEach((r) => body.appendChild(r));
});

// Table filters: a search box with data-filter="#table" matches row text;
// selects (or hidden inputs set by filter chips) with
// data-col-filter="#table" data-col="N" match one column exactly.
function applyFilters(sel) {
  const box = document.querySelector(`input[data-filter="${sel}"]`);
  const q = box ? box.value.trim().toLowerCase() : "";
  const cols = [...document.querySelectorAll(`[data-col-filter="${sel}"]`)].filter((s) => s.value);
  document.querySelectorAll(sel + " tbody tr").forEach((r) => {
    let show = q === "" || r.textContent.toLowerCase().includes(q);
    for (const s of cols) {
      const cell = r.children[+s.dataset.col];
      const v = cell ? (cell.dataset.value ?? cell.textContent.trim()) : "";
      if (v !== s.value) show = false;
    }
    r.hidden = !show;
  });
}
document.addEventListener("input", (e) => {
  const sel = e.target.dataset && (e.target.dataset.filter || e.target.dataset.colFilter);
  if (sel) applyFilters(sel);
});

// Filter chips: one pressed at a time, writing its value into the hidden
// column filter next to it.
document.addEventListener("click", (e) => {
  const chip = e.target.closest("[data-chip]");
  if (!chip) return;
  const group = chip.parentElement;
  const field = group.querySelector("[data-col-filter]");
  group.querySelectorAll("[data-chip]").forEach((c) => c.setAttribute("aria-pressed", String(c === chip)));
  field.value = chip.dataset.chip;
  applyFilters(field.dataset.colFilter);
});

// "/" focuses the search box on any list page.
document.addEventListener("keydown", (e) => {
  if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
  const t = e.target;
  if (t.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName)) return;
  const box = document.querySelector("input[data-filter], input[data-logs-filter]");
  if (box) {
    e.preventDefault();
    box.focus();
  }
});

// Confirm before every stack action, showing the exact command. Buttons
// with data-command open the dialog; without JavaScript the form just posts.
const dialog = document.getElementById("confirm-dialog");
let confirmed = null;
document.addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-command]");
  if (!btn || !dialog) return;
  if (confirmed === btn) { confirmed = null; return; }
  e.preventDefault();
  dialog.querySelector("[data-confirm-title]").textContent = btn.dataset.title || btn.textContent;
  dialog.querySelector("[data-confirm-command]").textContent = btn.dataset.command;
  dialog.classList.toggle("is-danger", btn.hasAttribute("data-danger"));
  const verb = btn.textContent.trim();
  dialog.querySelector("[data-confirm-ok]").textContent = verb ? verb[0].toUpperCase() + verb.slice(1) : "Run";
  dialog.querySelector("[data-confirm-ok]").classList.toggle("danger", btn.hasAttribute("data-danger"));
  dialog.querySelector("[data-confirm-ok]").classList.toggle("primary", !btn.hasAttribute("data-danger"));
  dialog.returnValue = "";
  dialog.onclose = () => {
    if (dialog.returnValue === "ok") { confirmed = btn; btn.click(); }
  };
  dialog.showModal();
});

// Live output for a running action, over a websocket.
for (const box of document.querySelectorAll("[data-job]")) {
  const out = box.querySelector(".job-out");
  const status = box.querySelector(".job-status");
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/jobs/${box.dataset.job}/ws`);
  ws.onopen = () => { status.textContent = "Running"; };
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.lines) {
      const stick = out.scrollTop + out.clientHeight >= out.scrollHeight - 4;
      const frag = document.createDocumentFragment();
      for (const line of msg.lines) {
        const div = document.createElement("div");
        // Commands Fleetling ran and its own notes stand apart from
        // what Compose printed.
        if (line.startsWith("$ ")) div.className = "run";
        else if (line.startsWith("fleetling: ")) div.className = "note";
        div.textContent = line;
        frag.append(div);
      }
      out.append(frag);
      if (stick) out.scrollTop = out.scrollHeight;
    }
    if (msg.done) {
      status.textContent = "";
      const dot = document.createElement("span");
      dot.className = "dot " + (msg.exit === 0 ? "dot-ok" : "dot-stopped");
      status.append(dot, msg.exit === 0 ? "Finished, exit 0. " : `Failed, exit ${msg.exit}. `);
      status.classList.toggle("warn", msg.exit !== 0);
      const a = document.createElement("a");
      a.href = location.pathname;
      a.textContent = "Reload status";
      status.appendChild(a);
    }
  };
  ws.onerror = () => { status.textContent = "Lost the connection. The action keeps running; reload to check."; };
}

// Blurred secrets on the env tab: click or Enter to show.
document.addEventListener("click", (e) => {
  const s = e.target.closest(".secret");
  if (s) s.classList.add("shown");
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target.classList && e.target.classList.contains("secret")) e.target.classList.add("shown");
});

for (const el of document.querySelectorAll("[data-logs]")) setupLogs(el);

// Network form: show only the fields that apply to the chosen driver.
for (const sel of document.querySelectorAll("select[data-driver]")) {
  const form = sel.closest("form");
  const sync = () => {
    form.querySelectorAll("[data-show-driver]").forEach((el) => {
      el.hidden = !el.dataset.showDriver.split(" ").includes(sel.value);
    });
  };
  sel.addEventListener("change", sync);
  sync();
}

// Light YAML colouring for read-only files: keys, quoted strings, comments
// and list dashes. Builds DOM nodes from the text; never parses HTML.
const YAML_LINE = /^(\s*)(- )?(?:("[^"]*"|'[^']*'|[^\s#'"][^:#]*?)(:)(?=\s|$))?(.*)$/;
function colourYaml(pre) {
  const frag = document.createDocumentFragment();
  const span = (cls, text) => {
    if (!text) return;
    if (!cls) { frag.append(text); return; }
    const s = document.createElement("span");
    s.className = cls;
    s.textContent = text;
    frag.append(s);
  };
  const lines = pre.textContent.split("\n");
  lines.forEach((line, i) => {
    const c = line.match(/^(\s*)(#.*)$/);
    if (c) { span("", c[1]); span("y-com", c[2]); }
    else {
      const m = line.match(YAML_LINE) || [line, "", "", "", "", line];
      span("", m[1]);
      span("y-pun", m[2]);
      span("y-key", m[3]);
      span("y-pun", m[4]);
      // The value: a quoted string, a trailing comment, or plain text.
      // Plain string scans only, so a huge line can't stall the page.
      let rest = m[5] || "";
      const lead = rest.length - rest.trimStart().length;
      span("", rest.slice(0, lead));
      rest = rest.slice(lead);
      const q = rest[0];
      if (q === '"' || q === "'") {
        const end = rest.indexOf(q, 1);
        const cut = end < 0 ? rest.length : end + 1;
        span("y-str", rest.slice(0, cut));
        rest = rest.slice(cut);
      }
      const hash = rest.search(/\s#/);
      if (hash < 0) span("", rest);
      else { span("", rest.slice(0, hash)); span("y-com", rest.slice(hash)); }
    }
    if (i < lines.length - 1) frag.append("\n");
  });
  pre.replaceChildren(frag);
}
for (const pre of document.querySelectorAll('pre[data-lang="yaml"]')) colourYaml(pre);
