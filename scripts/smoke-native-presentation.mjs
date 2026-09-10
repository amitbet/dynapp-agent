// Run in a signed-in macOS/Windows desktop after building the agent:
// node scripts/smoke-native-presentation.mjs /absolute/path/to/dynapp-shell-agent
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { createInterface } from "node:readline";
import { once } from "node:events";

const executable = process.argv[2];
if (!executable) throw new Error("Pass the built agent executable");
const helpers = [];
async function helper() {
  const token = randomBytes(32).toString("hex");
  let accept;
  const connected = new Promise((resolve) => { accept = resolve; });
  const server = createServer((socket) => {
    const lines = createInterface({ input: socket });
    lines.once("line", (line) => {
      // Local development tools may probe a newly opened HTTP-looking port.
      if (line !== token) { lines.close(); socket.destroy(); return; }
      accept({ socket, lines });
    });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const child = spawn(executable, ["presentation-helper", `127.0.0.1:${server.address().port}`], {
    env: { ...process.env, DYNAPP_PRESENTATION_TOKEN: token }, stdio: ["ignore", "ignore", "pipe"],
  });
  let diagnostics = "";
  child.stderr.on("data", (chunk) => { diagnostics += chunk; });
  const exited = once(child, "exit");
  const timeout = setTimeout(() => { child.kill(); server.close(); }, 10_000);
  let socket, lines;
  try {
    ({ socket, lines } = await Promise.race([connected, exited.then(() => { throw new Error(`Helper exited: ${diagnostics}`); })]));
  } finally { clearTimeout(timeout); server.close(); }
  const pending = new Map();
  let id = 0;
  lines.on("line", (line) => {
    if (line === token) return;
    const reply = JSON.parse(line);
    if (reply.id) { const request = pending.get(reply.id); pending.delete(reply.id); request?.(reply); }
  });
  const instance = {
    async call(service, method, args = []) {
      const requestID = String(++id);
      const reply = new Promise((resolve) => pending.set(requestID, resolve));
      socket.write(`${JSON.stringify({ id: requestID, service, method, args })}\n`);
      let timer;
      try {
        const result = await Promise.race([reply, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${service}.${method} timed out: ${diagnostics}`)), 10_000); })]);
        if (result.error) throw new Error(result.error);
        return result.result;
      } finally { clearTimeout(timer); }
    },
    async close() {
      socket.end();
      const timer = setTimeout(() => child.kill(), 3000);
      const [code] = await exited;
      clearTimeout(timer);
      assert.equal(code, 0, `helper should exit cleanly: ${diagnostics}`);
    },
  };
  helpers.push(instance);
  return instance;
}

try {
  const first = await helper();
  assert.equal(await first.call("tray", "create", [{ tooltip: "DynApp native smoke test", title: "", menu: [{ id: "check", label: "Test", type: "checkbox" }, { label: "More", submenu: [{ id: "nested", label: "Nested" }] }] }]), true);
  assert.equal(await first.call("tray", "setToolTip", ["Updated native smoke test"]), true);
  assert.equal(await first.call("tray", "setTitle", ["Test"]), true);
  const shortcut = "CommandOrControl+Alt+Shift+F19";
  assert.equal(await first.call("globalShortcut", "register", [shortcut]), true);
  const second = await helper();
  assert.equal(await second.call("globalShortcut", "register", [shortcut]), false, "OS prevents app collisions");
  await second.call("globalShortcut", "unregister", [shortcut]);
  assert.equal(await second.call("globalShortcut", "register", [shortcut]), false, "another app cannot unregister the owner");
  await first.close(); helpers.splice(helpers.indexOf(first), 1);
  assert.equal(await second.call("globalShortcut", "register", [shortcut]), true, "disconnect releases the native shortcut");
  await second.call("globalShortcut", "unregisterAll");
  await second.call("tray", "create", [{ tooltip: "Cleanup test" }]);
  await second.call("tray", "destroy");
  if (process.argv.includes("--screenshots")) {
    const displays = await second.call("screen", "listDisplays");
    assert.ok(displays.length > 0);
    const primary = displays.find((display) => display.primary);
    assert.ok(primary);
    const screenshot = await second.call("screen", "capture", [{ displayId: primary.id, region: { x: 0, y: 0, width: 64, height: 64 } }]);
    const png = Buffer.from(screenshot.dataUrl.split(",")[1], "base64");
    assert.equal(png.subarray(1, 4).toString(), "PNG");
    assert.equal(png.readUInt32BE(16), 64);
    assert.equal(png.readUInt32BE(20), 64);
    console.log("Native 64×64 screenshot returned a valid PNG.");
  }
  console.log("Native tray creation, updates, shortcut collision, app isolation, and disconnect cleanup passed.");
} finally {
  await Promise.all(helpers.map((instance) => instance.close()));
}
