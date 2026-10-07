// DynApp native host page bridge (docs/native-host.md in the dynapp repo).
// The host injects this into the top-level document before any page script
// runs. The agent serves it, so app bundles do not change with the bridge.
(() => {
  "use strict";
  const config = __DYNAPP_NATIVE_HOST_CONFIG__;
  if (window.top !== window || location.origin !== config.origin || window.__DYNAPP_NATIVE_HOST__) return;

  const post = config.platform === "windows"
    ? (message) => window.chrome.webview.postMessage(JSON.stringify({ dynappNativeHost: 1, ...message }))
    : (message) => window.webkit.messageHandlers.dynappNativeHost.postMessage({ dynappNativeHost: 1, ...message });

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
    return bytes.buffer;
  };

  const sockets = new Map();
  const reviews = new Map();
  let nextId = 0;
  // The host keeps its socket table across reloads, so ids must not repeat
  // between page loads: a reused id would be ignored (macOS) or would receive
  // the previous page's agent frames (Windows).
  const pageId = (() => {
    const bytes = new Uint8Array(8);
    if (window.crypto?.getRandomValues) window.crypto.getRandomValues(bytes);
    else for (let index = 0; index < bytes.length; index += 1) bytes[index] = Math.floor(Math.random() * 256);
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  })();

  // WebSocket-shaped agent connection. The PWA Shell uses it as the "native"
  // carrier of the local environment.
  class NativeAgentSocket {
    constructor() {
      this.readyState = 0;
      this.binaryType = "arraybuffer";
      this.onopen = null;
      this.onmessage = null;
      this.onerror = null;
      this.onclose = null;
      this.listeners = new Map();
      this.sid = `s${pageId}-${++nextId}`;
      sockets.set(this.sid, this);
      post({ op: "open", sid: this.sid });
    }

    addEventListener(type, listener, options) {
      if (typeof listener !== "function") return;
      const entries = this.listeners.get(type) ?? [];
      entries.push({ listener, once: Boolean(options && typeof options === "object" && options.once) });
      this.listeners.set(type, entries);
    }

    removeEventListener(type, listener) {
      const entries = this.listeners.get(type);
      if (entries) this.listeners.set(type, entries.filter((entry) => entry.listener !== listener));
    }

    emit(type, event) {
      const handler = this[`on${type}`];
      if (typeof handler === "function") {
        try { handler.call(this, event); } catch (error) { setTimeout(() => { throw error; }); }
      }
      for (const entry of [...(this.listeners.get(type) ?? [])]) {
        if (entry.once) this.removeEventListener(type, entry.listener);
        try { entry.listener.call(this, event); } catch (error) { setTimeout(() => { throw error; }); }
      }
    }

    send(data) {
      if (this.readyState !== 1) throw new Error("The DynApp agent connection is not open");
      if (typeof data === "string") {
        post({ op: "send", sid: this.sid, text: data });
        return;
      }
      let bytes;
      if (data instanceof ArrayBuffer) bytes = new Uint8Array(data);
      else if (ArrayBuffer.isView(data)) bytes = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
      else throw new TypeError("DynApp agent frames must be strings or bytes");
      post({ op: "send", sid: this.sid, binary: toBase64(bytes) });
    }

    close(code = 1000, reason = "") {
      if (this.readyState >= 2) return;
      this.readyState = 2;
      post({ op: "close", sid: this.sid, code: Number(code) || 1000, reason: String(reason || "") });
    }
  }

  const deliver = (message) => {
    if (!message || typeof message !== "object") return;
    if (message.event === "review") {
      const pending = reviews.get(message.rid);
      if (!pending) return;
      reviews.delete(message.rid);
      if (message.error) pending.reject(new Error(String(message.error)));
      else pending.resolve({ changed: Boolean(message.changed) });
      return;
    }
    if (message.event === "drop") {
      window.dispatchEvent(new CustomEvent("dynappnativedrop", {
        detail: { paths: Array.isArray(message.paths) ? message.paths.map(String) : [], x: Number(message.x), y: Number(message.y) },
      }));
      return;
    }
    const socket = sockets.get(message.sid);
    if (!socket) return;
    switch (message.event) {
      case "open":
        if (socket.readyState !== 0) return;
        socket.readyState = 1;
        socket.emit("open", { type: "open" });
        return;
      case "message":
        if (socket.readyState !== 1) return;
        socket.emit("message", { type: "message", data: typeof message.binary === "string" ? fromBase64(message.binary) : String(message.text ?? "") });
        return;
      case "error":
        socket.emit("error", { type: "error", error: new Error(String(message.error || "The DynApp agent connection failed")) });
        return;
      case "close": {
        sockets.delete(socket.sid);
        socket.readyState = 3;
        const code = Number(message.code) || 1006;
        socket.emit("close", { type: "close", code, reason: String(message.reason || ""), wasClean: code === 1000 });
        return;
      }
      default:
    }
  };

  // Release this page's agent connections when it goes away; otherwise they
  // stay open in the host until the app quits.
  window.addEventListener?.("pagehide", () => {
    for (const socket of sockets.values()) socket.close(1001, "Page unloaded");
    sockets.clear();
  });

  Object.defineProperty(window, "__dynappNativeHostDeliver", { value: deliver });
  Object.defineProperty(window, "__DYNAPP_NATIVE_HOST__", {
    value: Object.freeze({
      version: config.version,
      platform: config.platform,
      storeId: config.storeId,
      origin: config.origin,
      openAgentSocket: () => new NativeAgentSocket(),
      reviewPermissions: () => new Promise((resolve, reject) => {
        const rid = `r${pageId}-${++nextId}`;
        reviews.set(rid, { resolve, reject });
        post({ op: "review", rid });
      }),
    }),
  });
})();
