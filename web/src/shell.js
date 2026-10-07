// The web shell: xterm.js on a websocket to an exec with a TTY.
// Binary frames carry terminal bytes; text frames carry JSON control.
import "@xterm/xterm/css/xterm.css";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

// xterm.js injects <style> elements and has no option for a CSP nonce.
// Stamp this page's nonce on every <style> it creates, before it is
// inserted, so the strict style-src still holds. xterm only creates them
// in term.open(), which runs after this.
const nonce = document.querySelector('meta[name="csp-nonce"]')?.content;
if (nonce) {
  const create = Document.prototype.createElement;
  document.createElement = function (tag, opts) {
    const el = create.call(this, tag, opts);
    if (String(tag).toLowerCase() === "style") el.nonce = nonce;
    return el;
  };
}


const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();

for (const root of document.querySelectorAll("[data-shell]")) {
  const box = root.querySelector(".terminal");
  const status = root.querySelector(".shell-status");
  const connectBtn = root.querySelector("[data-shell-connect]");
  const closeBtn = root.querySelector("[data-shell-disconnect]");
  let term = null, ws = null, fit = null;

  const onWindowResize = () => fit && fit.fit();
  window.addEventListener("resize", onWindowResize);

  function setRunning(on) {
    connectBtn.hidden = on;
    closeBtn.hidden = !on;
  }

  connectBtn.addEventListener("click", () => {
    if (term) term.dispose();
    term = new Terminal({
      cursorBlink: true,
      fontFamily: css("--mono") || "monospace",
      fontSize: 13,
      // The terminal sits on the console surface in both themes.
      theme: {
        background: css("--console"), foreground: css("--console-ink"), cursor: css("--accent"),
        cursorAccent: css("--console"), selectionBackground: css("--console-sel"),
        black: "#1b2228", red: "#f0736a", green: "#57c98a", yellow: "#f2c14e", blue: "#6fa8f0",
        magenta: "#d58cf0", cyan: "#5cc8d6", white: "#d6dee4",
        brightBlack: "#6b7884", brightRed: "#ff958c", brightGreen: "#7fe0aa", brightYellow: "#ffd76e",
        brightBlue: "#94c1ff", brightMagenta: "#e8adff", brightCyan: "#86e2ec", brightWhite: "#ffffff",
      },
    });
    fit = new FitAddon();
    term.loadAddon(fit);
    term.open(box);
    fit.fit();

    const params = new URLSearchParams({
      cols: term.cols, rows: term.rows,
      cmd: root.querySelector('[name="cmd"]').value,
      user: root.querySelector('[name="user"]').value,
    });
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}${root.dataset.shell}?${params}`);
    ws.binaryType = "arraybuffer";
    status.textContent = "Connecting...";
    const enc = new TextEncoder();

    ws.onopen = () => { status.textContent = "Connected"; setRunning(true); term.focus(); };
    ws.onmessage = (ev) => {
      if (typeof ev.data !== "string") {
        term.write(new Uint8Array(ev.data));
        return;
      }
      const msg = JSON.parse(ev.data);
      if (msg.type === "exit") status.textContent = `Exited with code ${msg.code}`;
      if (msg.type === "error") {
        status.textContent = "Error";
        term.writeln("\r\n\x1b[31m" + msg.message + "\x1b[0m");
      }
    };
    ws.onclose = () => {
      setRunning(false);
      if (status.textContent === "Connected") status.textContent = "Closed";
    };
    term.onData((d) => { if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d)); });
    term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
    });
  });

  closeBtn.addEventListener("click", () => ws && ws.close());
}
