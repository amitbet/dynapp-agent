// Runs the native host page bridge in a fake page: node --test shellagent/
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const template = readFileSync(new URL("./native_host_bridge.js", import.meta.url), "utf8");
const origin = "https://amitbet-notes.dynapp.io";
// Objects made inside the vm context have that context's prototypes.
const plain = (value) => JSON.parse(JSON.stringify(value));

function page({ platform = "macos", pageOrigin = origin, top = true } = {}) {
  const posted = [];
  const events = [];
  const listeners = {};
  const window = {
    addEventListener(type, listener) { (listeners[type] ??= []).push(listener); },
    location: { origin: pageOrigin },
    btoa, atob, setTimeout,
    CustomEvent: class CustomEvent { constructor(type, init) { this.type = type; this.detail = init?.detail; } },
    dispatchEvent(event) { events.push(event); },
    webkit: { messageHandlers: { dynappNativeHost: { postMessage: (message) => posted.push(message) } } },
    chrome: { webview: { postMessage: (message) => posted.push(JSON.parse(message)) } },
  };
  if (platform === "android") {
    const replyListeners = [];
    window.dynappNativeHost = {
      postMessage: (message) => posted.push(JSON.parse(message)),
      addEventListener: (type, listener) => { if (type === "message") replyListeners.push(listener); },
      reply: (message) => { for (const listener of replyListeners) listener({ data: JSON.stringify(message) }); },
    };
  }
  if (platform === "android") {
    window.DOMException = class DOMException extends Error { constructor(message, name) { super(message); this.name = name; } };
    window.navigator = {
      userActivation: { isActive: true },
      clipboard: { readText: async () => "", writeText: async () => { throw new Error("Write permission denied."); } },
    };
  }
  window.window = window;
  window.top = top ? window : {};
  const config = JSON.stringify({ version: 1, platform, storeId: "amit-bet/notes", origin });
  vm.runInNewContext(template.replace("__DYNAPP_NATIVE_HOST_CONFIG__", config), window);
  const fire = (type) => { for (const listener of listeners[type] ?? []) listener({ type }); };
  return { window, posted, events, fire, host: window.__DYNAPP_NATIVE_HOST__, deliver: window.__dynappNativeHostDeliver };
}

test("the bridge installs only in the app's own top-level document", () => {
  assert.equal(page().host.storeId, "amit-bet/notes");
  assert.equal(page({ pageOrigin: "https://evil.example" }).host, undefined);
  assert.equal(page({ top: false }).host, undefined);
  const { host } = page();
  assert.equal(Object.isFrozen(host), true);
});

test("an agent socket opens, carries text and binary frames, and closes", () => {
  const { host, posted, deliver } = page();
  const socket = host.openAgentSocket();
  const opened = [];
  const received = [];
  let closed = null;
  socket.addEventListener("open", () => opened.push(true), { once: true });
  socket.addEventListener("message", (event) => received.push(event.data));
  socket.onclose = (event) => { closed = event; };
  assert.deepEqual(plain(posted[0]), { dynappNativeHost: 1, op: "open", sid: socket.sid });
  assert.throws(() => socket.send("too early"), /not open/);
  deliver({ sid: socket.sid, event: "open" });
  deliver({ sid: socket.sid, event: "open" });
  assert.equal(socket.readyState, 1);
  assert.equal(opened.length, 1);
  socket.send('{"type":"hello"}');
  socket.send(new Uint8Array([0, 1, 255]));
  assert.deepEqual(plain(posted[1]), { dynappNativeHost: 1, op: "send", sid: socket.sid, text: '{"type":"hello"}' });
  assert.equal(posted[2].binary, Buffer.from([0, 1, 255]).toString("base64"));
  deliver({ sid: socket.sid, event: "message", text: '{"type":"pong"}' });
  deliver({ sid: socket.sid, event: "message", binary: Buffer.from([7, 8]).toString("base64") });
  assert.equal(received[0], '{"type":"pong"}');
  assert.deepEqual([...new Uint8Array(received[1])], [7, 8]);
  deliver({ sid: socket.sid, event: "close", code: 1012, reason: "Permissions changed" });
  assert.equal(socket.readyState, 3);
  assert.equal(closed.code, 1012);
  assert.equal(closed.reason, "Permissions changed");
  deliver({ sid: socket.sid, event: "message", text: "late" });
  assert.equal(received.length, 2);
});

test("reviewPermissions resolves from the host answer", async () => {
  const { host, posted, deliver } = page();
  const pending = host.reviewPermissions();
  const request = posted.at(-1);
  assert.equal(request.op, "review");
  deliver({ rid: request.rid, event: "review", changed: true });
  assert.deepEqual(plain(await pending), { changed: true });
  const failed = host.reviewPermissions();
  deliver({ rid: posted.at(-1).rid, event: "review", error: "The DynApp agent is not running" });
  await assert.rejects(failed, /not running/);
});

test("host drops become dynappnativedrop events", () => {
  const { events, deliver } = page();
  deliver({ event: "drop", paths: ["/Users/me/a.txt"], x: 12, y: 34 });
  assert.equal(events[0].type, "dynappnativedrop");
  assert.deepEqual(plain(events[0].detail), { paths: ["/Users/me/a.txt"], x: 12, y: 34 });
});

test("the Windows bridge posts JSON strings through chrome.webview", () => {
  const { host, posted } = page({ platform: "windows" });
  const socket = host.openAgentSocket();
  assert.deepEqual(plain(posted[0]), { dynappNativeHost: 1, op: "open", sid: socket.sid });
});

test("socket ids differ between page loads so a reload cannot reuse the old page's connection", () => {
  const first = page().host.openAgentSocket();
  const second = page().host.openAgentSocket();
  assert.notEqual(first.sid, second.sid);
});

test("pagehide closes the page's open agent sockets", () => {
  const { host, posted, deliver, fire } = page();
  const open = host.openAgentSocket();
  const pending = host.openAgentSocket();
  const done = host.openAgentSocket();
  deliver({ sid: open.sid, event: "open" });
  deliver({ sid: done.sid, event: "close", code: 1000 });
  fire("pagehide");
  const closes = posted.filter((message) => message.op === "close").map((message) => message.sid);
  assert.deepEqual(closes.sort(), [open.sid, pending.sid].sort());
});

test("android posts strings to the injected host object and takes replies from it", () => {
  const { window, host, posted } = page({ platform: "android" });
  assert.equal(host.platform, "android");
  const socket = host.openAgentSocket();
  assert.deepEqual(plain(posted.at(-1)), { dynappNativeHost: 1, op: "open", sid: socket.sid });
  const received = [];
  socket.addEventListener("message", (event) => received.push(event.data));
  window.dynappNativeHost.reply({ sid: socket.sid, event: "open" });
  window.dynappNativeHost.reply({ sid: socket.sid, event: "message", text: "hi" });
  assert.equal(socket.readyState, 1);
  assert.deepEqual(received, ["hi"]);
});

test("android does not install the bridge without the origin-scoped host object", () => {
  const listeners = {};
  const window = { addEventListener(type, listener) { (listeners[type] ??= []).push(listener); }, location: { origin } };
  window.window = window;
  window.top = window;
  const config = JSON.stringify({ version: 1, platform: "android", storeId: "amit-bet/notes", origin });
  vm.runInNewContext(template.replace("__DYNAPP_NATIVE_HOST_CONFIG__", config), window);
  assert.equal(window.__DYNAPP_NATIVE_HOST__, undefined);
});

test("android provides Notification and clipboard reads through the host", async () => {
  const { window, posted } = page({ platform: "android" });
  const query = posted.find((message) => message.op === "notificationPermission");
  assert.equal(query.query, true);
  window.dynappNativeHost.reply({ rid: query.rid, event: "result", value: "default" });
  const permission = window.Notification.requestPermission();
  const ask = posted.at(-1);
  assert.equal(ask.op, "notificationPermission");
  assert.equal(ask.query, undefined);
  window.dynappNativeHost.reply({ rid: ask.rid, event: "result", value: "granted" });
  assert.equal(await permission, "granted");
  assert.equal(window.Notification.permission, "granted");
  new window.Notification("Done", { body: "Export finished", tag: "export" });
  assert.deepEqual(plain(posted.at(-1)), { dynappNativeHost: 1, op: "notify", title: "Done", body: "Export finished", tag: "export", silent: false });

  const text = window.navigator.clipboard.readText();
  const read = posted.at(-1);
  assert.equal(read.op, "clipboardReadText");
  window.dynappNativeHost.reply({ rid: read.rid, event: "result", error: "Clipboard access was not allowed" });
  await assert.rejects(text, /not allowed/);
});

test("android notifications stay silent until the host grants them", () => {
  const { window, posted } = page({ platform: "android" });
  new window.Notification("Early");
  assert.equal(posted.some((message) => message.op === "notify"), false);
});

test("android clipboard writes fall back to the host only during a user gesture", async () => {
  const { window, posted } = page({ platform: "android" });
  const write = window.navigator.clipboard.writeText("copied");
  await new Promise((resolve) => setTimeout(resolve, 0));
  const request = posted.at(-1);
  assert.equal(request.op, "clipboardWriteText");
  assert.equal(request.text, "copied");
  window.dynappNativeHost.reply({ rid: request.rid, event: "result", value: true });
  await write;
  window.navigator.userActivation.isActive = false;
  await assert.rejects(window.navigator.clipboard.writeText("sneaky"), /Write permission denied/);
  assert.equal(posted.filter((message) => message.op === "clipboardWriteText").length, 1);
});
