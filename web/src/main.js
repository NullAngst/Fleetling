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

// Remove page: the command line follows the ticked options, the list of
// what gets deleted follows the ticked paths in the order the server
// deletes them (deepest first, the stack folder last), and the confirm
// field shows once anything needs it. The server checks all of it again.
for (const form of document.querySelectorAll("form[data-remove]")) {
  const cmd = form.querySelector("[data-remove-cmd]");
  const then = form.querySelector("[data-remove-then]");
  const list = form.querySelector("[data-remove-list]");
  const confirmBox = form.querySelector("[data-remove-confirm]");
  const input = confirmBox.querySelector("input");
  const submit = form.querySelector("[data-remove-submit]");
  const depth = (p) => p.split("/").length;
  const sync = () => {
    const flags = [...form.querySelectorAll("input[data-flag]:checked")].map((i) => i.dataset.flag);
    cmd.textContent = [form.dataset.baseCmd, ...flags].join(" ");
    const paths = [...form.querySelectorAll('input[name="path"]:checked')].map((i) => i.value);
    paths.sort((a, b) => {
      if (a === form.dataset.stackDir) return 1;
      if (b === form.dataset.stackDir) return -1;
      return depth(b) - depth(a) || (a < b ? -1 : 1);
    });
    list.replaceChildren(...paths.map((p) => {
      const li = document.createElement("li");
      li.textContent = p;
      return li;
    }));
    then.hidden = paths.length === 0;
    const needs = paths.length > 0 || !!form.querySelector('input[name="volumes"]:checked');
    confirmBox.hidden = !needs;
    submit.disabled = needs && input.value.trim() !== form.dataset.name;
  };
  form.addEventListener("change", sync);
  form.addEventListener("input", sync);
  sync();
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

// Self-update: poll /healthz until a different process answers, showing
// the helper's log while waiting. Fetch errors are expected while the
// container is being replaced, so they only update the status text.
for (const box of document.querySelectorAll("[data-self-update]")) {
  const status = box.querySelector("[data-self-status]");
  const logEl = box.querySelector("[data-self-log]");
  const boot = box.dataset.boot;
  const started = Date.now();
  const tick = async () => {
    try {
      const r = await fetch("/healthz", { cache: "no-store" });
      const now = r.headers.get("X-Fleetling-Boot");
      if (r.ok && now && now !== boot) {
        status.textContent = "Back. Opening the log...";
        location.href = "/self/last-update?done=1";
        return;
      }
      status.textContent = "The old Fleetling is still answering";
      const l = await fetch("/self/log", { cache: "no-store" });
      if (l.ok) {
        logEl.textContent = await l.text();
        logEl.scrollTop = logEl.scrollHeight;
      }
    } catch {
      status.textContent = "Fleetling is restarting, retrying...";
    }
    // Builds from the repo can take a while; give up polling after 30 minutes.
    if (Date.now() - started < 30 * 60 * 1000) setTimeout(tick, 2000);
    else status.textContent = "Still not back after 30 minutes. Check the helper from the host.";
  };
  setTimeout(tick, 1500);
}

// New stack: suggest an engine from the compose text. The server reads the
// file for Podman-only settings and says which line it found. A choice the
// user made by hand is never overridden.
{
  const composeBox = document.querySelector('form[action="/stacks/new"] textarea[name="compose"]');
  const why = document.querySelector("[data-engine-why]");
  const sel = document.querySelector("select[data-engine-suggest]");
  if (composeBox && why) {
    if (sel) sel.addEventListener("change", () => { sel.dataset.touched = "1"; });
    const csrf = document.querySelector('meta[name="csrf-token"]');
    let timer = null;
    const check = async () => {
      try {
        const r = await fetch("/stacks/detect", {
          method: "POST",
          headers: { "Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": csrf ? csrf.content : "" },
          body: new URLSearchParams({ compose: composeBox.value }),
        });
        if (!r.ok) return;
        const rep = await r.json();
        why.textContent = rep.why;
        why.hidden = !rep.why;
        if (sel && !sel.dataset.touched) {
          const def = sel.dataset.default || (sel.dataset.default = sel.options[0].value);
          sel.value = rep.engine || def;
        }
      } catch { /* offline; leave the form as it is */ }
    };
    composeBox.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(check, 500); });
  }
}

// Stack build page: the command under the button follows the toggles, so
// the confirm always shows exactly what runs.
for (const form of document.querySelectorAll("[data-build-preview]")) {
  const out = form.querySelector("[data-build-command]");
  const btn = form.querySelector("button[data-command]");
  const flags = [...form.querySelectorAll("[data-build-flag]")];
  const update = () => {
    const pull = flags.find((f) => f.dataset.buildFlag === "pull").checked;
    const nc = flags.find((f) => f.dataset.buildFlag === "nocache").checked;
    const v = form.querySelector(`[data-build-variant="${pull},${nc}"]`);
    if (v) { out.textContent = v.value; btn.dataset.command = v.value; }
  };
  flags.forEach((f) => f.addEventListener("change", update));
}

// Tabs that switch panels on the same page (the converter). Without
// JavaScript every panel shows and the tabs are plain anchors.
for (const nav of document.querySelectorAll("[data-tabs]")) {
  const links = [...nav.querySelectorAll("a[href^='#']")];
  const show = (id) => {
    links.forEach((a) => {
      const on = a.getAttribute("href") === "#" + id;
      if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
      const panel = document.getElementById(a.getAttribute("href").slice(1));
      if (panel) panel.hidden = !on;
    });
  };
  links.forEach((a) => a.addEventListener("click", (e) => { e.preventDefault(); show(a.getAttribute("href").slice(1)); }));
  show(links[0].getAttribute("href").slice(1));
}

// Copy buttons copy the <pre> in their pane.
document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  const pre = btn.closest(".pane")?.querySelector("pre");
  if (!pre) return;
  try {
    await navigator.clipboard.writeText(pre.textContent);
    btn.textContent = "Copied";
  } catch {
    // Clipboard needs a secure context; select the text instead.
    const range = document.createRange();
    range.selectNodeContents(pre);
    const sel = getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    btn.textContent = "Selected";
  }
  setTimeout(() => { btn.textContent = "Copy"; }, 1500);
});

// Converter: picking a stack hides the paste box.
for (const sel of document.querySelectorAll("[data-convert-source]")) {
  const paste = document.querySelector("[data-convert-paste]");
  sel.addEventListener("change", () => { if (paste) paste.hidden = sel.value !== ""; });
}
