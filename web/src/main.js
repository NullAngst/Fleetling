// Fleetling's browser code: HTMX plus a few small helpers. Bundled by
// esbuild into web/static/dist/app.js. No inline scripts anywhere, so the
// CSP can stay at script-src 'self'.
import htmx from "htmx.org";

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

// Search boxes with data-filter="#table" hide rows that don't match.
document.addEventListener("input", (e) => {
  const sel = e.target.dataset && e.target.dataset.filter;
  if (!sel) return;
  const q = e.target.value.trim().toLowerCase();
  document.querySelectorAll(sel + " tbody tr").forEach((r) => {
    r.hidden = q !== "" && !r.textContent.toLowerCase().includes(q);
  });
});

// "/" focuses the search box on any list page.
document.addEventListener("keydown", (e) => {
  if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
  const t = e.target;
  if (t.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName)) return;
  const box = document.querySelector("input[data-filter]");
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
      out.textContent += msg.lines.join("\n") + "\n";
      if (stick) out.scrollTop = out.scrollHeight;
    }
    if (msg.done) {
      status.textContent = msg.exit === 0 ? "Finished, exit 0. " : `Failed, exit ${msg.exit}. `;
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
