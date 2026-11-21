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
      buffer.waiting = true;
      serveRead(buffer, message.stream, deliver);
      break;
    }
    case "wt-close":
      deliver({ event: "closed", code: message.code, reason: message.reason });
      break;
  }
}

// Like the Go host, a read request blocks until data or the end arrives.
function serveRead(buffer, stream, deliver) {
  if (!buffer.waiting) return;
  if (buffer.data.length) deliver({ stream, event: "data", binary: buffer.data.shift() });
  else if (buffer.fin) deliver({ stream, event: "end" });
  else return;
  buffer.waiting = false;
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
