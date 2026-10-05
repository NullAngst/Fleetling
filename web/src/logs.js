// The log viewer for containers and stacks: streams over a websocket,
// keeps ANSI colors, filters on the client, wraps or not, and downloads
// what it has.

const MAX_LINES = 20000;

// ANSI SGR colors to class names. Anything else (cursor moves and such)
// is dropped. Text always goes in through textContent, never as HTML.
const SGR = /\x1b\[([0-9;]*)m/g;
const OTHER_ESC = /\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07/g;

function renderAnsi(text, into) {
  let state = { fg: null, bold: false };
  let last = 0;
  const push = (chunk) => {
    if (!chunk) return;
    const clean = chunk.replace(OTHER_ESC, "");
    if (state.fg === null && !state.bold) {
      into.appendChild(document.createTextNode(clean));
      return;
    }
    const span = document.createElement("span");
    if (state.fg !== null) span.className = "ansi-" + state.fg;
    if (state.bold) span.classList.add("ansi-bold");
    span.textContent = clean;
    into.appendChild(span);
  };
  for (const m of text.matchAll(SGR)) {
    push(text.slice(last, m.index));
    last = m.index + m[0].length;
    const codes = m[1] === "" ? [0] : m[1].split(";").map(Number);
    for (const c of codes) {
      if (c === 0) state = { fg: null, bold: false };
      else if (c === 1) state.bold = true;
      else if (c === 22) state.bold = false;
      else if (c === 39) state.fg = null;
      else if (c >= 30 && c <= 37) state.fg = c - 30;
      else if (c >= 90 && c <= 97) state.fg = c - 90 + 8;
    }
  }
  push(text.slice(last));
}

const stripAnsi = (t) => t.replace(SGR, "").replace(OTHER_ESC, "");

export function setupLogs(root) {
  const out = root.querySelector(".logs-out");
  const status = root.querySelector(".logs-status");
  const field = (n) => root.querySelector(`[name="${n}"]`);
  const filter = field("filter");
  let ws = null;
  let lines = []; // plain text, for filter and download

  const matches = (t) => {
    const q = filter.value.trim().toLowerCase();
    return q === "" || t.toLowerCase().includes(q);
  };

  function append(batch) {
    const stick = out.scrollTop + out.clientHeight >= out.scrollHeight - 8;
    const frag = document.createDocumentFragment();
    for (const l of batch) {
      const div = document.createElement("div");
      if (l.e) div.className = "stderr";
      renderAnsi(l.t, div);
      const plain = stripAnsi(l.t);
      div.hidden = !matches(plain);
      frag.appendChild(div);
      lines.push(plain);
    }
    out.appendChild(frag);
    while (lines.length > MAX_LINES) {
      lines.shift();
      out.firstChild.remove();
    }
    if (stick) out.scrollTop = out.scrollHeight;
  }

  function connect() {
    if (ws) { ws.onclose = null; ws.close(); }
    out.textContent = "";
    lines = [];
    const params = new URLSearchParams({
      tail: field("tail").value,
      since: field("since").value,
      follow: field("follow").checked ? "1" : "0",
      timestamps: field("timestamps").checked ? "1" : "0",
    });
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}${root.dataset.logs}?${params}`);
    status.textContent = "Connecting...";
    ws.onopen = () => { status.textContent = field("follow").checked ? "Following" : "Loading"; };
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.lines) append(msg.lines);
      if (msg.done) status.textContent = msg.error ? "Stopped: " + msg.error : "End of log";
    };
    ws.onclose = () => { if (status.textContent.startsWith("Follow")) status.textContent = "Disconnected"; };
  }

  for (const n of ["tail", "follow", "timestamps"]) field(n).addEventListener("change", connect);
  field("since").addEventListener("change", connect);
  field("wrap").addEventListener("change", () => out.classList.toggle("wrap", field("wrap").checked));
  filter.addEventListener("input", () => {
    [...out.children].forEach((d, i) => { d.hidden = !matches(lines[i] ?? d.textContent); });
  });
  root.querySelector("[data-logs-download]").addEventListener("click", () => {
    const blob = new Blob([lines.join("\n") + "\n"], { type: "text/plain" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = (document.title.split(" - ")[0] || "logs").replace(/[^\w.-]+/g, "_") + ".log";
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  });
  connect();
}
