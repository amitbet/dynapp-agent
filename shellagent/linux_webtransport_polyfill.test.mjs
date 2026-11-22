// Runs the Linux host's WebTransport stand-in in a fake page against a fake
// host that echoes each stream: node --test shellagent/
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const polyfill = readFileSync(new URL("./linuxhost/webtransport_polyfill.js", import.meta.url), "utf8");

function page({ nativeWebTransport = false, host = echoHost } = {}) {
  const posted = [];
  const context = {
    location: { href: "https://amitbet-formfuel.dynapp.io/", origin: "https://amitbet-formfuel.dynapp.io" },
    URL, btoa, atob, crypto, setTimeout, ReadableStream, WritableStream, TypeError, SyntaxError,
    webkit: { messageHandlers: { dynappNativeHost: { postMessage: (raw) => { const message = JSON.parse(raw); posted.push(message); host(message, context); } } } },
  };
  context.window = context;
  context.top = context;
  if (nativeWebTransport) context.WebTransport = function NativeWebTransport() {};
  vm.createContext(context);
  vm.runInContext(`const __dynappHostToken = "launch-token";\n${polyfill}`, context);
  return { window: context, posted };
}

// The fake host answers asynchronously, like the GTK main loop.
function echoHost(message, window) {
  const deliver = (event) => setTimeout(() => window.__dynappWebTransportDeliver({ wid: message.wid, ...event }), 0);
  const buffers = (echoHost.buffers ??= new Map());
  const key = `${message.wid}/${message.stream}`;
  switch (message.op) {
    case "wt-open":
      if (message.url.includes("refuse")) deliver({ event: "failed", error: "server certificate does not match serverCertificateHashes" });
      else deliver({ event: "ready" });
      break;
    case "wt-stream":
      buffers.set(key, { data: [], fin: false });
      deliver({ stream: message.stream, event: "stream-ready" });
      break;
    case "wt-write":
      buffers.get(key).data.push(message.binary);
      deliver({ stream: message.stream, event: "ack" });
      serveRead(buffers.get(key), message.stream, deliver);
      break;
    case "wt-fin":
      buffers.get(key).fin = true;
      deliver({ stream: message.stream, event: "fin" });
      serveRead(buffers.get(key), message.stream, deliver);
      break;
    case "wt-read": {
      const buffer = buffers.get(key);
      buffer.waiting = (buffer.waiting || 0) + 1;
      serveRead(buffer, message.stream, deliver);
      break;
    }
    case "wt-close":
      deliver({ event: "closed", code: message.code, reason: message.reason });
      break;
  }
}

// Like the Go host, each read request blocks until data or the end arrives.
function serveRead(buffer, stream, deliver) {
  while (buffer.waiting) {
    if (buffer.data.length) deliver({ stream, event: "data", binary: buffer.data.shift() });
    else if (buffer.fin) { deliver({ stream, event: "end" }); buffer.waiting = 0; return; }
    else return;
    buffer.waiting -= 1;
  }
}

const hash = { algorithm: "sha-256", value: new Uint8Array(32).fill(7) };

test("the stand-in posts signed open messages with base64 certificate hashes", () => {
  const { window, posted } = page();
  new window.WebTransport("https://192.168.1.81:9011/dynapp-remote", { serverCertificateHashes: [hash] });
  const open = posted.find((message) => message.op === "wt-open");
  assert.equal(open.hostToken, "launch-token");
  assert.equal(open.dynappNativeHost, 1);
  assert.equal(open.url, "https://192.168.1.81:9011/dynapp-remote");
  assert.deepEqual(open.hashes, [Buffer.alloc(32, 7).toString("base64")]);
  assert.equal(posted[0].op, "wt-reset", "a new document first drops the previous page's sessions");
});

test("a bidirectional stream writes, ends, and reads back through the host", async () => {
  const { window } = page();
  const transport = new window.WebTransport("https://192.168.1.81:9011/dynapp-remote", { serverCertificateHashes: [hash] });
  await transport.ready;
  const { readable, writable } = await transport.createBidirectionalStream();
  const writer = writable.getWriter();
  await writer.write(new TextEncoder().encode("hello "));
  await writer.write(new Uint8Array([76, 65, 78]).buffer);
  await writer.close();
  const reader = readable.getReader();
  let text = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    text += new TextDecoder().decode(value);
  }
  assert.equal(text, "hello LAN");
  transport.close({ closeCode: 4, reason: "done" });
  assert.deepEqual({ ...(await transport.closed) }, { closeCode: 4, reason: "done" });
});

test("a refused handshake rejects ready and closed with a WebTransportError", async () => {
  const { window } = page();
  const transport = new window.WebTransport("https://192.168.1.81:9011/refuse", { serverCertificateHashes: [hash] });
  await assert.rejects(transport.ready, (error) => error.name === "WebTransportError" && /serverCertificateHashes/.test(error.message));
  await assert.rejects(transport.closed, /serverCertificateHashes/);
  await assert.rejects(transport.createBidirectionalStream(), /serverCertificateHashes/);
});

test("a server close fails open streams and resolves closed with its code", async () => {
  let window;
  const host = (message, context) => {
    if (message.op === "wt-open") setTimeout(() => context.__dynappWebTransportDeliver({ wid: message.wid, event: "ready" }), 0);
    if (message.op === "wt-stream") setTimeout(() => context.__dynappWebTransportDeliver({ wid: message.wid, stream: message.stream, event: "stream-ready" }), 0);
    if (message.op === "wt-read") setTimeout(() => context.__dynappWebTransportDeliver({ wid: message.wid, event: "closed", code: 4401, reason: "pairing required" }), 0);
  };
  ({ window } = page({ host }));
  const transport = new window.WebTransport("https://10.0.0.5:9011/dynapp-remote", { serverCertificateHashes: [hash] });
  await transport.ready;
  const { readable } = await transport.createBidirectionalStream();
  await assert.rejects(readable.getReader().read(), (error) => error.closeCode === 4401);
  assert.deepEqual({ ...(await transport.closed) }, { closeCode: 4401, reason: "pairing required" });
});

test("the stand-in stays out of the way when the engine has WebTransport", () => {
  const { window, posted } = page({ nativeWebTransport: true });
  assert.equal(window.WebTransport.name, "NativeWebTransport");
  assert.equal(posted.length, 0);
});

test("several writes and reads are in flight at once, within the window", async () => {
  // A host that holds acks and data until the test releases them.
  const held = [];
  let readsOut = 0;
  const { window, posted } = page({
    host(message, context) {
      const deliver = (event) => context.__dynappWebTransportDeliver({ wid: message.wid, ...event });
      if (message.op === "wt-open") setTimeout(() => deliver({ event: "ready" }), 0);
      else if (message.op === "wt-stream") setTimeout(() => deliver({ stream: message.stream, event: "stream-ready" }), 0);
      else if (message.op === "wt-write") held.push(() => deliver({ stream: message.stream, event: "ack" }));
      else if (message.op === "wt-fin") held.push(() => deliver({ stream: message.stream, event: "fin" }));
      else if (message.op === "wt-read") { readsOut += 1; held.push(() => { readsOut -= 1; deliver({ stream: message.stream, event: "data", binary: btoa("x") }); }); }
    },
  });
  const transport = new window.WebTransport("https://192.168.1.81:9011/dynapp-remote", { serverCertificateHashes: [hash] });
  await transport.ready;
  const { readable, writable } = await transport.createBidirectionalStream();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(readsOut, 4, "the page keeps four reads out before any data arrives");

  const writer = writable.getWriter();
  const chunk = new Uint8Array(256 * 1024);
  const results = [];
  for (let index = 0; index < 5; index += 1) writer.write(chunk).then(() => results.push(index));
  await new Promise((resolve) => setTimeout(resolve, 0));
  const writes = posted.filter((message) => message.op === "wt-write").length;
  assert.equal(writes, 4, "1 MiB of writes goes out before any ack; the next waits");
  assert.deepEqual(results, [0, 1, 2], "writes resolve while under 1 MiB is unacknowledged");

  // Release every held ack and read; then the writer and reader catch up.
  const release = async () => { while (held.length) held.shift()(); await new Promise((resolve) => setTimeout(resolve, 0)); };
  while (results.length < 5) await release();
  assert.deepEqual(results, [0, 1, 2, 3, 4]);
  const reader = readable.getReader();
  for (let index = 0; index < 4; index += 1) assert.equal(new TextDecoder().decode((await reader.read()).value), "x");
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.ok(readsOut > 0 && readsOut <= 4, `reads are requested again as the reader drains (${readsOut})`);
  const closing = writer.close();
  await release();
  await closing;
});

test("a write error rejects the waiting write and later ones", async () => {
  const { window } = page({
    host(message, context) {
      const deliver = (event) => setTimeout(() => context.__dynappWebTransportDeliver({ wid: message.wid, ...event }), 0);
      if (message.op === "wt-open") deliver({ event: "ready" });
      else if (message.op === "wt-stream") deliver({ stream: message.stream, event: "stream-ready" });
      else if (message.op === "wt-write") deliver({ stream: message.stream, event: "write-error", error: "stream reset by peer" });
    },
  });
  const transport = new window.WebTransport("https://192.168.1.81:9011/dynapp-remote", { serverCertificateHashes: [hash] });
  await transport.ready;
  const { writable } = await transport.createBidirectionalStream();
  const writer = writable.getWriter();
  await writer.write(new Uint8Array(10)); // under the window: resolves before the host answers
  await new Promise((resolve) => setTimeout(resolve, 5));
  await assert.rejects(writer.write(new Uint8Array(10)), /stream reset by peer/);
});
