// WebTransport stand-in for the Linux app host (webtransport.go). WebKitGTK
// has no WebTransport; the host opens the QUIC sessions and this script
// gives the page the subset the PWA Shell uses: the constructor with
// serverCertificateHashes, ready, closed, close(), and
// createBidirectionalStream(). There are no datagrams; callers check for
// them. It runs inside the host's token closure (WrapBootstrap), so only it
// and the agent bridge can post to the host.
(() => {
  "use strict";
  if (typeof window.WebTransport === "function" || window.top !== window) return;
  const handler = window.webkit?.messageHandlers?.dynappNativeHost;
  if (!handler || typeof __dynappHostToken !== "string" || !__dynappHostToken) return;
  const token = __dynappHostToken;
  const stringify = JSON.stringify;
  const post = (message) => handler.postMessage(stringify({ dynappNativeHost: 1, ...message, hostToken: token }));

  const toBase64 = (bytes) => {
    let text = "";
    for (let index = 0; index < bytes.length; index += 0x8000) {
      text += String.fromCharCode.apply(null, bytes.subarray(index, index + 0x8000));
    }
    return btoa(text);
  };
  const fromBase64 = (text) => {
    const decoded = atob(text);
    const bytes = new Uint8Array(decoded.length);
    for (let index = 0; index < decoded.length; index += 1) bytes[index] = decoded.charCodeAt(index);
    return bytes;
  };
  const toBytes = (value) => {
    if (value instanceof Uint8Array) return value;
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    const tag = Object.prototype.toString.call(value);
    if (tag === "[object ArrayBuffer]" || tag === "[object SharedArrayBuffer]") return new Uint8Array(value);
    throw new TypeError("WebTransport streams accept BufferSource chunks");
  };
  const pageId = (() => {
    const bytes = new Uint8Array(8);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  })();
  let nextId = 0;
  const sessions = new Map();

  class WebTransportError extends Error {
    constructor(message, { closeCode = null, reason = "", source = "session" } = {}) {
      super(message);
      this.name = "WebTransportError";
      this.closeCode = closeCode;
      this.reason = reason;
      this.source = source;
      this.streamErrorCode = null;
    }
  }

  class WebTransport {
    #id;
    #state = "connecting";
    #streams = new Map();
    #resolveReady;
    #rejectReady;
    #resolveClosed;
    #rejectClosed;

    constructor(url, options = {}) {
      const target = new URL(String(url), location.href);
      if (target.protocol !== "https:") throw new SyntaxError("WebTransport needs an https URL");
      const hashes = Array.from(options?.serverCertificateHashes ?? [], (hash) => {
        if (String(hash?.algorithm ?? "").toLowerCase() !== "sha-256") throw new TypeError("serverCertificateHashes supports sha-256 only");
        return toBase64(toBytes(hash.value));
      });
      this.#id = `wt${pageId}-${++nextId}`;
      this.ready = new Promise((resolve, reject) => { this.#resolveReady = resolve; this.#rejectReady = reject; });
      this.closed = new Promise((resolve, reject) => { this.#resolveClosed = resolve; this.#rejectClosed = reject; });
      // Callers that only await one of the two must not see an unhandled
      // rejection from the other.
      this.ready.catch(() => {});
      this.closed.catch(() => {});
      this.datagrams = undefined;
      sessions.set(this.#id, this);
      post({ op: "wt-open", wid: this.#id, url: target.href, hashes });
    }

    get id() { return this.#id; }

    async createBidirectionalStream() {
      await this.ready;
      if (this.#state !== "connected") throw new WebTransportError("WebTransport session is closed");
      const stream = new HostStream(this.#id, `${this.#id}-s${++nextId}`);
      this.#streams.set(stream.id, stream);
      post({ op: "wt-stream", wid: this.#id, stream: stream.id });
      await stream.opened;
      return { readable: stream.readable, writable: stream.writable };
    }

    close({ closeCode = 0, reason = "" } = {}) {
      if (this.#state === "closed" || this.#state === "failed") return;
      post({ op: "wt-close", wid: this.#id, code: Number(closeCode) >>> 0, reason: String(reason) });
      if (this.#state === "connecting") {
        this.#fail(new WebTransportError("WebTransport session was closed before it connected"));
      } else {
        this.#finish(Number(closeCode) >>> 0, String(reason));
      }
    }

    deliver(event) {
      if (event.stream) {
        const stream = this.#streams.get(event.stream);
        if (stream) stream.deliver(event);
        if (stream?.done) this.#streams.delete(event.stream);
        return;
      }
      switch (event.event) {
        case "ready":
          if (this.#state === "connecting") { this.#state = "connected"; this.#resolveReady(undefined); }
          break;
        case "failed":
          this.#fail(new WebTransportError(event.error || "WebTransport connection failed"));
          break;
        case "closed":
          this.#finish(Number(event.code) || 0, String(event.reason || ""));
          break;
      }
    }

    #fail(error) {
      if (this.#state === "closed" || this.#state === "failed") return;
      this.#state = "failed";
      sessions.delete(this.#id);
      this.#rejectReady(error);
      this.#rejectClosed(error);
      for (const stream of this.#streams.values()) stream.fail(error);
      this.#streams.clear();
    }

    #finish(closeCode, reason) {
      if (this.#state === "closed" || this.#state === "failed") return;
      this.#state = "closed";
      sessions.delete(this.#id);
      const error = new WebTransportError("WebTransport session closed", { closeCode, reason });
      for (const stream of this.#streams.values()) stream.fail(error);
      this.#streams.clear();
      this.#resolveClosed({ closeCode, reason });
    }
  }

  class HostStream {
    constructor(wid, id) {
      this.wid = wid;
      this.id = id;
      this.done = false;
      this.readEnded = false;
      this.writeEnded = false;
      this.pendingRead = null;
      this.pendingWrites = [];
      this.opened = new Promise((resolve, reject) => { this.resolveOpen = resolve; this.rejectOpen = reject; });
      this.opened.catch(() => {});
      const stream = this;
      this.readable = new ReadableStream({
        // The first pull runs as soon as the stream object exists; the host
        // ignores reads for a stream it has not opened yet.
        pull(controller) {
          return stream.opened.then(() => {
            if (stream.readEnded) return undefined;
            return new Promise((resolve, reject) => {
              stream.pendingRead = { controller, resolve, reject };
              post({ op: "wt-read", wid: stream.wid, stream: stream.id });
            });
          });
        },
        cancel() {
          stream.readEnded = true;
          stream.abort();
        },
      }, { highWaterMark: 1 });
      this.writable = new WritableStream({
        write(chunk) {
          const bytes = toBytes(chunk);
          return new Promise((resolve, reject) => {
            stream.pendingWrites.push({ resolve, reject });
            post({ op: "wt-write", wid: stream.wid, stream: stream.id, binary: toBase64(bytes) });
          });
        },
        close() {
          return new Promise((resolve, reject) => {
            stream.pendingWrites.push({ resolve, reject, fin: true });
            post({ op: "wt-fin", wid: stream.wid, stream: stream.id });
          });
        },
        abort() {
          stream.writeEnded = true;
          stream.abort();
        },
      });
    }

    abort() {
      if (this.done) return;
      this.done = true;
      post({ op: "wt-abort", wid: this.wid, stream: this.id });
    }

    deliver(event) {
      switch (event.event) {
        case "stream-ready":
          this.resolveOpen();
          break;
        case "stream-error":
          this.fail(new WebTransportError(event.error || "Failed to create bidirectional stream.", { source: "stream" }));
          break;
        case "data": {
          const read = this.pendingRead;
          this.pendingRead = null;
          if (!read) break;
          try { read.controller.enqueue(fromBase64(event.binary || "")); } catch { /* cancelled */ }
          read.resolve();
          break;
        }
        case "end": {
          this.readEnded = true;
          const read = this.pendingRead;
          this.pendingRead = null;
          if (read) {
            try { read.controller.close(); } catch { /* cancelled */ }
            read.resolve();
          }
          this.settle();
          break;
        }
        case "read-error": {
          this.readEnded = true;
          const read = this.pendingRead;
          this.pendingRead = null;
          read?.reject(new WebTransportError(event.error || "WebTransport stream reset", { source: "stream" }));
          this.settle();
          break;
        }
        case "ack":
        case "fin": {
          const write = this.pendingWrites.shift();
          if (event.event === "fin") this.writeEnded = true;
          write?.resolve();
          this.settle();
          break;
        }
        case "write-error": {
          this.writeEnded = true;
          const error = new WebTransportError(event.error || "WebTransport stream write failed", { source: "stream" });
          for (const write of this.pendingWrites.splice(0)) write.reject(error);
          this.settle();
          break;
        }
      }
    }

    settle() {
      if (this.readEnded && this.writeEnded) this.done = true;
    }

    fail(error) {
      this.done = true;
      this.readEnded = true;
      this.writeEnded = true;
      this.rejectOpen(error);
      const read = this.pendingRead;
      this.pendingRead = null;
      read?.reject(error);
      for (const write of this.pendingWrites.splice(0)) write.reject(error);
    }
  }

  Object.defineProperty(window, "__dynappWebTransportDeliver", {
    value: (event) => {
      const session = event && sessions.get(String(event.wid || ""));
      if (session) session.deliver(event);
    },
    configurable: false,
    enumerable: false,
    writable: false,
  });
  Object.defineProperty(window, "WebTransport", { value: WebTransport, configurable: true, enumerable: false, writable: true });
  Object.defineProperty(window, "WebTransportError", { value: WebTransportError, configurable: true, enumerable: false, writable: true });
  // Sessions of the previous document are gone; let the host drop them.
  post({ op: "wt-reset" });
})();
