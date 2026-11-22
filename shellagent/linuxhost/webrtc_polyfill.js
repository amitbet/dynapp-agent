// RTCPeerConnection stand-in for the Linux app host (webrtc.go).
// Distribution builds of WebKitGTK have no WebRTC; pion in the host runs the
// peer connection and this script gives the page the data-channel subset
// the PWA Shell uses: createDataChannel, createOffer, set{Local,Remote}
// Description, addIceCandidate, getStats, close, the icecandidate,
// icegatheringstatechange, iceconnectionstatechange, connectionstatechange
// and datachannel events, and data channels with open/message/close/error.
// There are no media tracks. It runs inside the host's token closure
// (WrapBootstrap), like the agent bridge.
(() => {
  "use strict";
  if (typeof window.RTCPeerConnection === "function" || window.top !== window) return;
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
    return bytes.buffer;
  };
  const toBytes = (value) => {
    if (value instanceof Uint8Array) return value;
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    const tag = Object.prototype.toString.call(value);
    if (tag === "[object ArrayBuffer]" || tag === "[object SharedArrayBuffer]") return new Uint8Array(value);
    return null;
  };
  const encoder = new TextEncoder();
  const pageId = (() => {
    const bytes = new Uint8Array(8);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  })();
  let nextId = 0;
  const peers = new Map();

  class Emitter {
    #listeners = new Map();
    addEventListener(type, listener, options) {
      if (typeof listener !== "function") return;
      const list = this.#listeners.get(type) ?? [];
      list.push({ listener, once: Boolean(options && typeof options === "object" && options.once) });
      this.#listeners.set(type, list);
    }
    removeEventListener(type, listener) {
      const list = this.#listeners.get(type);
      if (list) this.#listeners.set(type, list.filter((entry) => entry.listener !== listener));
    }
    dispatch(type, fields = {}) {
      const event = { type, target: this, currentTarget: this, ...fields };
      const handler = this[`on${type}`];
      if (typeof handler === "function") {
        try { handler.call(this, event); } catch (error) { setTimeout(() => { throw error; }); }
      }
      const list = this.#listeners.get(type) ?? [];
      this.#listeners.set(type, list.filter((entry) => !entry.once));
      for (const { listener } of list) {
        try { listener.call(this, event); } catch (error) { setTimeout(() => { throw error; }); }
      }
      return true;
    }
  }

  const candidateObject = (init) => {
    if (!init) return null;
    const json = {
      candidate: String(init.candidate ?? ""),
      sdpMid: init.sdpMid ?? null,
      sdpMLineIndex: init.sdpMLineIndex ?? null,
      usernameFragment: init.usernameFragment ?? null,
    };
    return { ...json, toJSON: () => ({ ...json }) };
  };
  const descriptionObject = (value) => (value && value.type ? { type: String(value.type), sdp: String(value.sdp ?? "") } : null);

  // bufferedAmount is bytes the page posted that the host has not handed to
  // pion yet, plus what pion still queues; the host reports both in
  // "buffered" events.
  class RTCDataChannel extends Emitter {
    #posted = 0;
    #acked = 0;
    #queued = 0;
    #seq = 0;
    #threshold = 0;

    constructor(peer, id, label, { ordered = true, maxRetransmits = null } = {}) {
      super();
      this.peer = peer;
      this.id = null;
      this.channelId = id;
      this.label = String(label);
      this.ordered = ordered !== false;
      this.maxRetransmits = maxRetransmits;
      this.readyState = "connecting";
      this.binaryType = "blob";
      this.onopen = null;
      this.onbufferedamountlow = null;
      this.onmessage = null;
      this.onclose = null;
      this.onerror = null;
    }

    get bufferedAmount() {
      return Math.max(0, this.#posted - this.#acked) + this.#queued;
    }

    get bufferedAmountLowThreshold() {
      return this.#threshold;
    }

    set bufferedAmountLowThreshold(value) {
      const threshold = Math.max(0, Math.floor(Number(value)) || 0);
      if (threshold === this.#threshold) return;
      this.#threshold = threshold;
      if (this.readyState !== "closed") post({ op: "rtc-threshold", pid: this.peer, cid: this.channelId, size: threshold });
    }

    send(data) {
      if (this.readyState !== "open") {
        const error = new Error("RTCDataChannel is not open");
        error.name = "InvalidStateError";
        throw error;
      }
      if (typeof data === "string") {
        const size = encoder.encode(data).byteLength;
        this.#posted += size;
        post({ op: "rtc-send", pid: this.peer, cid: this.channelId, text: data, size });
        return;
      }
      const bytes = toBytes(data);
      if (!bytes) throw new TypeError("RTCDataChannel.send accepts strings and BufferSource");
      this.#posted += bytes.byteLength;
      post({ op: "rtc-send", pid: this.peer, cid: this.channelId, binary: toBase64(bytes), isBinary: true, size: bytes.byteLength });
    }

    close() {
      if (this.readyState === "closing" || this.readyState === "closed") return;
      this.readyState = "closing";
      post({ op: "rtc-channel-close", pid: this.peer, cid: this.channelId });
    }

    deliver(event) {
      switch (event.event) {
        case "open":
          if (this.readyState === "connecting") {
            this.readyState = "open";
            this.dispatch("open");
          }
          break;
        case "message": {
          let data;
          if (typeof event.text === "string") data = event.text;
          else {
            const buffer = fromBase64(event.binary || "");
            data = this.binaryType === "arraybuffer" || typeof Blob !== "function" ? buffer : new Blob([buffer]);
          }
          this.dispatch("message", { data });
          break;
        }
        case "buffered": {
          const seq = Number(event.seq) || 0;
          if (seq <= this.#seq) break;
          this.#seq = seq;
          const before = this.bufferedAmount;
          this.#acked = Math.max(this.#acked, Number(event.acked) || 0);
          this.#queued = Math.max(0, Number(event.amount) || 0);
          if (before > this.#threshold && this.bufferedAmount <= this.#threshold && this.readyState !== "closed") this.dispatch("bufferedamountlow");
          break;
        }
        case "channel-error":
          this.dispatch("error", { error: new Error(event.error || "RTCDataChannel failed") });
          break;
        case "close":
          this.finish();
          break;
      }
    }

    finish() {
      if (this.readyState === "closed") return;
      this.readyState = "closed";
      this.dispatch("close");
    }
  }

  class RTCPeerConnection extends Emitter {
    #id;
    #channels = new Map();
    #requests = new Map();

    constructor(configuration = {}) {
      super();
      this.#id = `rtc${pageId}-${++nextId}`;
      this.connectionState = "new";
      this.iceConnectionState = "new";
      this.iceGatheringState = "new";
      this.signalingState = "stable";
      this.localDescription = null;
      this.remoteDescription = null;
      this.onicecandidate = null;
      this.onicegatheringstatechange = null;
      this.oniceconnectionstatechange = null;
      this.onconnectionstatechange = null;
      this.ondatachannel = null;
      const iceServers = Array.from(configuration?.iceServers ?? [], (server) => ({
        urls: Array.isArray(server?.urls) ? server.urls.map(String) : String(server?.urls ?? ""),
        username: server?.username ? String(server.username) : "",
        credential: server?.credential ? String(server.credential) : "",
      }));
      peers.set(this.#id, this);
      post({ op: "rtc-new", pid: this.#id, iceServers });
    }

    createDataChannel(label, init = {}) {
      if (this.connectionState === "closed") {
        const error = new Error("RTCPeerConnection is closed");
        error.name = "InvalidStateError";
        throw error;
      }
      const id = `c${++nextId}`;
      const maxRetransmits = Number.isInteger(init?.maxRetransmits) ? init.maxRetransmits : null;
      const channel = new RTCDataChannel(this.#id, id, label, { ordered: init?.ordered !== false, maxRetransmits });
      this.#channels.set(id, channel);
      const message = { op: "rtc-channel", pid: this.#id, cid: id, label: String(label ?? ""), ordered: init?.ordered !== false };
      if (maxRetransmits !== null) message.maxRetransmits = maxRetransmits;
      post(message);
      return channel;
    }

    #request(op, fields = {}) {
      if (this.connectionState === "closed") return Promise.reject(new Error("RTCPeerConnection is closed"));
      const rid = `r${++nextId}`;
      return new Promise((resolve, reject) => {
        this.#requests.set(rid, { resolve, reject });
        post({ op, pid: this.#id, rid, ...fields });
      });
    }

    async createOffer() {
      return descriptionObject(await this.#request("rtc-offer"));
    }

    async setLocalDescription(description) {
      const value = await this.#request("rtc-local", { sdpType: description?.type ?? "", sdp: description?.sdp ?? "" });
      this.#applyDescriptions(value);
    }

    async setRemoteDescription(description) {
      if (!description?.type) throw new TypeError("setRemoteDescription needs a session description");
      const value = await this.#request("rtc-remote", { sdpType: description.type, sdp: description.sdp ?? "" });
      this.#applyDescriptions(value);
    }

    async addIceCandidate(candidate) {
      // A missing or empty candidate marks the end of remote candidates.
      if (!candidate || !candidate.candidate) return;
      const init = typeof candidate.toJSON === "function" ? candidate.toJSON() : candidate;
      await this.#request("rtc-candidate", { candidate: {
        candidate: String(init.candidate),
        sdpMid: init.sdpMid ?? null,
        sdpMLineIndex: init.sdpMLineIndex ?? null,
        usernameFragment: init.usernameFragment ?? null,
      } });
    }

    async getStats() {
      const stats = await this.#request("rtc-stats");
      return new Map((Array.isArray(stats) ? stats : []).filter((stat) => stat?.id).map((stat) => [stat.id, stat]));
    }

    close() {
      if (this.connectionState === "closed") return;
      post({ op: "rtc-close", pid: this.#id });
      this.#finish();
    }

    #applyDescriptions(value) {
      if (value && "localDescription" in value) this.localDescription = descriptionObject(value.localDescription);
      if (value && "remoteDescription" in value) this.remoteDescription = descriptionObject(value.remoteDescription);
    }

    #finish() {
      if (this.connectionState === "closed") return;
      peers.delete(this.#id);
      this.connectionState = "closed";
      this.iceConnectionState = "closed";
      this.signalingState = "closed";
      const error = new Error("RTCPeerConnection is closed");
      for (const { reject } of this.#requests.values()) reject(error);
      this.#requests.clear();
      for (const channel of this.#channels.values()) channel.finish();
      this.#channels.clear();
    }

    deliver(event) {
      if (event.cid && event.event !== "datachannel") {
        const channel = this.#channels.get(event.cid);
        if (channel) {
          channel.deliver(event);
          if (channel.readyState === "closed") this.#channels.delete(event.cid);
        }
        return;
      }
      if ("localDescription" in event) this.localDescription = descriptionObject(event.localDescription);
      switch (event.event) {
        case "result": {
          const request = this.#requests.get(event.rid);
          this.#requests.delete(event.rid);
          if (!request) break;
          if (event.error) request.reject(new Error(event.error));
          else request.resolve(event.value ?? null);
          break;
        }
        case "icecandidate":
          this.dispatch("icecandidate", { candidate: candidateObject(event.candidate) });
          break;
        case "icegatheringstatechange":
          this.iceGatheringState = event.state;
          this.dispatch("icegatheringstatechange");
          break;
        case "iceconnectionstatechange":
          if (this.connectionState === "closed") break;
          this.iceConnectionState = event.state;
          this.dispatch("iceconnectionstatechange");
          break;
        case "connectionstatechange":
          if (this.connectionState === "closed") break;
          this.connectionState = event.state;
          this.dispatch("connectionstatechange");
          if (event.state === "closed") this.#finish();
          break;
        case "failed":
          this.connectionState = "failed";
          this.dispatch("connectionstatechange");
          break;
        case "datachannel": {
          const channel = new RTCDataChannel(this.#id, event.cid, event.label, { ordered: event.ordered !== false });
          this.#channels.set(event.cid, channel);
          this.dispatch("datachannel", { channel });
          break;
        }
      }
    }
  }

  Object.defineProperty(window, "__dynappRTCDeliver", {
    value: (event) => {
      const peer = event && peers.get(String(event.pid || ""));
      if (peer) peer.deliver(event);
    },
    configurable: false,
    enumerable: false,
    writable: false,
  });
  Object.defineProperty(window, "RTCPeerConnection", { value: RTCPeerConnection, configurable: true, enumerable: false, writable: true });
  // Peer connections of the previous document are gone; let the host drop them.
  post({ op: "rtc-reset" });
})();
