// Runs the Linux host's RTCPeerConnection stand-in in a fake page against a
// scripted host: node --test shellagent/
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const polyfill = readFileSync(new URL("./linuxhost/webrtc_polyfill.js", import.meta.url), "utf8");

function page(host) {
  const posted = [];
  const context = {
    btoa, atob, crypto, setTimeout, Map, TypeError, Blob,
    webkit: { messageHandlers: { dynappNativeHost: { postMessage: (raw) => { const message = JSON.parse(raw); posted.push(message); host?.(message, deliver); } } } },
  };
  const deliver = (event) => setTimeout(() => context.__dynappRTCDeliver(event), 0);
  context.window = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(`const __dynappHostToken = "launch-token";\n${polyfill}`, context);
  return { window: context, posted, deliver };
}

const offerSDP = "v=0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n";

// Answers requests like the Go bridge and plays the connection forward.
function scriptedHost(message, deliver) {
  const { pid, rid } = message;
  switch (message.op) {
    case "rtc-offer":
      deliver({ pid, rid, event: "result", value: { type: "offer", sdp: offerSDP } });
      break;
    case "rtc-local":
      deliver({ pid, rid, event: "result", value: { localDescription: { type: "offer", sdp: offerSDP }, remoteDescription: null } });
      deliver({ pid, event: "icegatheringstatechange", state: "gathering" });
      deliver({ pid, event: "icecandidate", candidate: { candidate: "candidate:1 1 udp 1 192.168.1.5 5000 typ host", sdpMid: "0", sdpMLineIndex: 0 }, localDescription: { type: "offer", sdp: `${offerSDP}a=candidate:1\r\n` } });
      deliver({ pid, event: "icecandidate", candidate: null, localDescription: { type: "offer", sdp: `${offerSDP}a=candidate:1\r\n` } });
      deliver({ pid, event: "icegatheringstatechange", state: "complete", localDescription: { type: "offer", sdp: `${offerSDP}a=candidate:1\r\n` } });
      break;
    case "rtc-remote":
      deliver({ pid, rid, event: "result", value: { localDescription: { type: "offer", sdp: offerSDP }, remoteDescription: { type: message.sdpType, sdp: message.sdp } } });
      deliver({ pid, event: "connectionstatechange", state: "connected" });
      deliver({ pid, cid: "c2", event: "open" });
      break;
    case "rtc-send":
      deliver({ pid, cid: message.cid, event: "message", ...(message.isBinary ? { binary: message.binary } : { text: `echo:${message.text}` }) });
      break;
    case "rtc-candidate":
      deliver({ pid, rid, event: "result", value: null });
      break;
    case "rtc-stats":
      deliver({ pid, rid, event: "result", value: [{ id: "P", type: "candidate-pair", state: "succeeded", nominated: true, localCandidateId: "L", remoteCandidateId: "R" }, { id: "L", type: "local-candidate", candidateType: "host" }] });
      break;
  }
}

test("the stand-in signs its messages and passes iceServers to the host", () => {
  const { window, posted } = page();
  new window.RTCPeerConnection({ iceServers: [{ urls: "stun:stun.cloudflare.com:3478" }, { urls: ["turn:t.example:3478"], username: "u", credential: "c" }] });
  assert.equal(posted[0].op, "rtc-reset");
  const created = posted.find((message) => message.op === "rtc-new");
  assert.equal(created.hostToken, "launch-token");
  assert.deepEqual(created.iceServers, [
    { urls: "stun:stun.cloudflare.com:3478", username: "", credential: "" },
    { urls: ["turn:t.example:3478"], username: "u", credential: "c" },
  ]);
});

test("offer, gathering, answer, open, and echo follow the browser API", async () => {
  const { window, posted } = page(scriptedHost);
  const pc = new window.RTCPeerConnection({ iceServers: [] });
  const control = pc.createDataChannel("dynapp-control-v2", { ordered: true });
  pc.createDataChannel("dynapp-datagram-v2", { ordered: false, maxRetransmits: 0 });
  const datagram = posted.find((message) => message.label === "dynapp-datagram-v2");
  assert.equal(datagram.ordered, false);
  assert.equal(datagram.maxRetransmits, 0);
  const candidates = [];
  pc.addEventListener("icecandidate", (event) => candidates.push(event.candidate && event.candidate.toJSON()));
  const offer = await pc.createOffer();
  assert.equal(offer.type, "offer");
  await pc.setLocalDescription(offer);
  await new Promise((resolve) => pc.addEventListener("icegatheringstatechange", () => { if (pc.iceGatheringState === "complete") resolve(); }));
  assert.equal(candidates.length, 2);
  assert.equal(candidates[0].candidate.includes("typ host"), true);
  assert.equal(candidates[1], null, "a null candidate ends gathering");
  assert.match(pc.localDescription.sdp, /a=candidate/, "localDescription carries the gathered candidates");

  const opened = new Promise((resolve) => control.addEventListener("open", resolve, { once: true }));
  await pc.setRemoteDescription({ type: "answer", sdp: "v=0\r\n" });
  assert.equal(pc.remoteDescription.type, "answer");
  await opened;
  assert.equal(control.readyState, "open");
  assert.equal(pc.connectionState, "connected");
  await pc.addIceCandidate({ candidate: "candidate:2 1 udp 1 10.0.0.2 6000 typ host", sdpMid: "0", sdpMLineIndex: 0 });
  await pc.addIceCandidate(null);

  control.binaryType = "arraybuffer";
  const messages = [];
  control.onmessage = (event) => messages.push(event.data);
  control.send("hello");
  control.send(new Uint8Array([1, 2, 3]));
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(messages[0], "echo:hello");
  assert.deepEqual([...new Uint8Array(messages[1])], [1, 2, 3]);

  const stats = await pc.getStats();
  assert.equal(stats.get("P").state, "succeeded");
  assert.equal([...stats.values()].length, 2);

  pc.close();
  assert.equal(pc.connectionState, "closed");
  assert.equal(control.readyState, "closed");
  await assert.rejects(pc.createOffer(), /closed/);
  assert.equal(posted.at(-1).op, "rtc-close");
});

test("sending on a channel that is not open throws InvalidStateError", () => {
  const { window } = page();
  const pc = new window.RTCPeerConnection();
  const channel = pc.createDataChannel("x");
  assert.throws(() => channel.send("early"), (error) => error.name === "InvalidStateError");
});

test("a failed host request rejects the call", async () => {
  const { window } = page((message, deliver) => {
    if (message.op === "rtc-remote") deliver({ pid: message.pid, rid: message.rid, event: "result", error: "unknown session description type" });
  });
  const pc = new window.RTCPeerConnection();
  await assert.rejects(pc.setRemoteDescription({ type: "bogus", sdp: "" }), /unknown session description type/);
});

test("the stand-in stays out of the way when the engine has WebRTC", () => {
  const posted = [];
  const context = { RTCPeerConnection: function NativePeer() {}, webkit: { messageHandlers: { dynappNativeHost: { postMessage: (raw) => posted.push(raw) } } } };
  context.window = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(`const __dynappHostToken = "t";\n${polyfill}`, context);
  assert.equal(context.RTCPeerConnection.name, "NativePeer");
  assert.equal(posted.length, 0);
});
