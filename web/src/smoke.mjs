// Browser smoke test: drives a running Fleetling in headless Chrome and
// fails on anything that suggests a frontend package update broke the UI:
// a console error, an uncaught exception, a missing asset, or a page whose
// script never set itself up (htmx, the CodeMirror editor, the xterm shell).
//
// The build workflow runs it against a fresh server. To run it by hand:
//
//   FLEETLING_URL=http://127.0.0.1:8420 FLEETLING_SETUP_TOKEN=... \
//   SMOKE_STACK=smoke SMOKE_CONTAINER=<container id> \
//   CHROME_PATH=/usr/bin/google-chrome node smoke.mjs
//
// The server must be fresh (not set up yet), SMOKE_STACK a folder under
// its root holding a compose.yaml, and SMOKE_CONTAINER a running container
// with /bin/sh on the Docker endpoint.

import puppeteer from "puppeteer-core";

const need = (k) => {
  const v = process.env[k];
  if (!v) {
    console.error(`smoke: ${k} is not set`);
    process.exit(2);
  }
  return v;
};
const base = need("FLEETLING_URL").replace(/\/$/, "");
const token = need("FLEETLING_SETUP_TOKEN");
const stack = need("SMOKE_STACK");
const ctr = need("SMOKE_CONTAINER");
const chrome = need("CHROME_PATH");
const password = "smoke test password, long enough";

const problems = [];
let where = "start";
const problem = (msg) => problems.push(`${where}: ${msg}`);

const browser = await puppeteer.launch({
  executablePath: chrome,
  headless: true,
  args: ["--no-sandbox", "--disable-dev-shm-usage"],
});
const page = await browser.newPage();
page.setDefaultTimeout(15000);
page.on("console", (m) => {
  if (m.type() === "error") problem(`console error: ${m.text()}`);
});
page.on("pageerror", (e) => problem(`uncaught: ${e.message}`));
page.on("requestfailed", (r) => {
  // A websocket or fetch cut short by leaving the page is not a fault.
  const why = r.failure()?.errorText || "";
  if (!why.includes("ERR_ABORTED")) problem(`request failed: ${r.url()} ${why}`);
});
page.on("response", (r) => {
  if (r.url().startsWith(base + "/static/") && r.status() >= 400) problem(`asset ${r.url()} answered ${r.status()}`);
});

async function visit(path) {
  where = path;
  const res = await page.goto(base + path, { waitUntil: "networkidle0" });
  if (!res || res.status() >= 400) problem(`answered ${res ? res.status() : "nothing"}`);
  if (!(await page.evaluate(() => typeof window.htmx === "object"))) problem("htmx did not load");
}

async function step(name, fn) {
  where = name;
  try {
    await fn();
  } catch (e) {
    problem(e.message);
  }
}

await step("setup", async () => {
  await visit("/setup");
  await page.type('input[name="token"]', token);
  await page.type('input[name="password"]', password);
  await page.type('input[name="confirm"]', password);
  await Promise.all([page.waitForNavigation({ waitUntil: "networkidle0" }), page.click('form[action="/setup"] button[type="submit"], form[action="/setup"] button')]);
  if (new URL(page.url()).pathname !== "/") throw new Error(`setup ended on ${page.url()}`);
});

await step("manage", async () => {
  await visit(`/stacks/${stack}`);
  await Promise.all([page.waitForNavigation({ waitUntil: "networkidle0" }), page.click(`form[action="/stacks/${stack}/manage"] button`)]);
});

for (const path of ["/", "/containers", "/networks", "/networks/new", "/images", "/volumes", "/builds", "/convert", "/updates", "/about", "/actions", "/import", "/settings", `/stacks/${stack}`, `/stacks/${stack}?tab=logs`]) {
  await step(path, () => visit(path));
}

// The editor pages: CodeMirror has to mount on every data-editor textarea.
for (const path of ["/stacks/new", `/stacks/${stack}/edit`]) {
  await step(path, async () => {
    await visit(path);
    const want = await page.$$eval("textarea[data-editor]", (els) => els.length);
    await page.waitForFunction((n) => document.querySelectorAll(".cm-editor").length >= n, {}, want);
    if (want === 0) throw new Error("no editor on the page");
  });
}

// The shell: xterm mounts, the websocket connects, and a command's output
// comes back.
await step("shell", async () => {
  await visit(`/containers/docker/${ctr}?tab=shell`);
  await page.click("[data-shell-connect]");
  await page.waitForSelector(".xterm");
  await new Promise((r) => setTimeout(r, 1000));
  await page.keyboard.type("echo smoke-$((40+2))\n");
  await page.waitForFunction(() => document.querySelector(".xterm")?.textContent.includes("smoke-42"));
  await page.click("[data-shell-disconnect]");
});

await browser.close();
if (problems.length) {
  console.error(`smoke: ${problems.length} problem(s)`);
  for (const p of problems) console.error("  " + p);
  process.exit(1);
}
console.log("smoke: every page loaded clean");
