package linuxhost

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// rtcEvents collects bridge events and lets a test wait for one.
type rtcEvents struct {
	mu     sync.Mutex
	events []map[string]any
	signal chan struct{}
}

func newRTCEvents() *rtcEvents { return &rtcEvents{signal: make(chan struct{}, 1024)} }

func (e *rtcEvents) deliver(event map[string]any) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
	e.signal <- struct{}{}
}

func (e *rtcEvents) wait(t *testing.T, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		e.mu.Lock()
		for index, event := range e.events {
			if match(event) {
				e.events = append(e.events[:index], e.events[index+1:]...)
				e.mu.Unlock()
				return event
			}
		}
		e.mu.Unlock()
		select {
		case <-e.signal:
		case <-deadline:
			t.Fatalf("no matching event; have %v", e.events)
		}
	}
}

func event(name string) func(map[string]any) bool {
	return func(e map[string]any) bool { return e["event"] == name }
}

func result(rid string) func(map[string]any) bool {
	return func(e map[string]any) bool { return e["event"] == "result" && e["rid"] == rid }
}

// answerer is the far side, like the agent's ICE answerer: it echoes every
// message on every channel the bridge opens.
func answerer(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		channel.OnMessage(func(message webrtc.DataChannelMessage) {
			if message.IsString {
				_ = channel.SendText("echo:" + string(message.Data))
			} else {
				_ = channel.Send(message.Data)
			}
		})
	})
	return pc
}

func TestRTCBridgeConnectsAndEchoesOverDataChannels(t *testing.T) {
	events := newRTCEvents()
	bridge := &RTCBridge{Deliver: events.deliver}
	t.Cleanup(bridge.Shutdown)
	ordered := true
	bridge.Handle(BridgeMessage{Op: "rtc-new", PID: "p1"})
	bridge.Handle(BridgeMessage{Op: "rtc-channel", PID: "p1", CID: "control", Label: "dynapp-control-v2", Ordered: &ordered})
	unordered := false
	none := 0
	bridge.Handle(BridgeMessage{Op: "rtc-channel", PID: "p1", CID: "datagrams", Label: "dynapp-datagram-v2", Ordered: &unordered, MaxRetransmits: &none})

	bridge.Handle(BridgeMessage{Op: "rtc-offer", PID: "p1", RID: "r1"})
	offer := events.wait(t, result("r1"))["value"].(map[string]any)
	if offer["type"] != "offer" || !strings.Contains(offer["sdp"].(string), "webrtc-datachannel") {
		t.Fatalf("offer = %v", offer)
	}
	bridge.Handle(BridgeMessage{Op: "rtc-local", PID: "p1", RID: "r2", SDPType: "offer", SDP: offer["sdp"].(string)})
	if failed := events.wait(t, result("r2")); failed["error"] != nil {
		t.Fatalf("setLocalDescription: %v", failed["error"])
	}
	// The page waits for gathering, then posts the localDescription it was
	// given with the final candidate event.
	gathered := events.wait(t, func(e map[string]any) bool { return e["event"] == "icecandidate" && e["candidate"] == nil })
	local := gathered["localDescription"].(map[string]any)
	if !strings.Contains(local["sdp"].(string), "a=candidate") {
		t.Fatalf("local description has no candidates: %v", local["sdp"])
	}

	far := answerer(t)
	if err := far.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: local["sdp"].(string)}); err != nil {
		t.Fatal(err)
	}
	answer, err := far.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := webrtc.GatheringCompletePromise(far)
	if err := far.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-done
	bridge.Handle(BridgeMessage{Op: "rtc-remote", PID: "p1", RID: "r3", SDPType: "answer", SDP: far.LocalDescription().SDP})
	if failed := events.wait(t, result("r3")); failed["error"] != nil {
		t.Fatalf("setRemoteDescription: %v", failed["error"])
	}
	// An end-of-candidates marker is accepted and ignored.
	end, _ := json.Marshal(map[string]any{"candidate": ""})
	bridge.Handle(BridgeMessage{Op: "rtc-candidate", PID: "p1", RID: "r4", Candidate: end})
	if failed := events.wait(t, result("r4")); failed["error"] != nil {
		t.Fatalf("addIceCandidate(end): %v", failed["error"])
	}

	events.wait(t, func(e map[string]any) bool { return e["event"] == "connectionstatechange" && e["state"] == "connected" })
	events.wait(t, func(e map[string]any) bool { return e["event"] == "open" && e["cid"] == "control" })

	bridge.Handle(BridgeMessage{Op: "rtc-send", PID: "p1", CID: "control", Text: `{"type":"hello"}`, Size: 16})
	if text := events.wait(t, func(e map[string]any) bool { return e["event"] == "message" && e["cid"] == "control" })["text"]; text != `echo:{"type":"hello"}` {
		t.Fatalf("text echo = %v", text)
	}
	payload := make([]byte, 200*1024)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	bridge.Handle(BridgeMessage{Op: "rtc-send", PID: "p1", CID: "control", Text: `{"type":"hello"}`, Size: 16})
	bridge.Handle(BridgeMessage{Op: "rtc-threshold", PID: "p1", CID: "control", Size: 64 * 1024})
	bridge.Handle(BridgeMessage{Op: "rtc-send", PID: "p1", CID: "control", Binary: base64.StdEncoding.EncodeToString(payload), IsBinary: true, Size: len(payload)})
	// Each send is acknowledged with the page's running byte count; pion's
	// low-queue callback adds reports in between. seq only grows.
	var lastSeq uint64
	var final map[string]any
	for _, want := range []uint64{16, 32, 32 + uint64(len(payload))} {
		for {
			report := events.wait(t, func(e map[string]any) bool { return e["event"] == "buffered" && e["cid"] == "control" })
			if seq := report["seq"].(uint64); seq <= lastSeq {
				t.Fatalf("seq went from %d to %d", lastSeq, seq)
			}
			lastSeq = report["seq"].(uint64)
			if report["acked"].(uint64) > want {
				t.Fatalf("acked %v before reaching %d", report["acked"], want)
			}
			if report["acked"] == want {
				final = report
				break
			}
		}
	}
	// If pion still queued part of the 200 KiB message, it reports again
	// once its queue falls to the page's threshold.
	for final["amount"].(uint64) > 64*1024 {
		final = events.wait(t, func(e map[string]any) bool { return e["event"] == "buffered" && e["cid"] == "control" })
	}
	echoed := events.wait(t, func(e map[string]any) bool {
		return e["event"] == "message" && e["cid"] == "control" && e["binary"] != nil
	})
	if data, _ := base64.StdEncoding.DecodeString(echoed["binary"].(string)); len(data) != len(payload) || data[1000] != payload[1000] {
		t.Fatalf("binary echo: %d bytes", len(data))
	}

	// A channel opened after the connection is up, like a per-chunk stream.
	bridge.Handle(BridgeMessage{Op: "rtc-channel", PID: "p1", CID: "stream-1", Label: "dynapp-stream-v2-1", Ordered: &ordered})
	events.wait(t, func(e map[string]any) bool { return e["event"] == "open" && e["cid"] == "stream-1" })

	bridge.Handle(BridgeMessage{Op: "rtc-stats", PID: "p1", RID: "r5"})
	stats := events.wait(t, result("r5"))["value"]
	encoded, _ := json.Marshal(stats)
	if !strings.Contains(string(encoded), `"type":"candidate-pair"`) || !strings.Contains(string(encoded), `"candidateType":"host"`) {
		t.Fatalf("stats lack the candidate pair: %s", encoded)
	}

	bridge.Handle(BridgeMessage{Op: "rtc-close", PID: "p1"})
	events.wait(t, func(e map[string]any) bool { return e["event"] == "connectionstatechange" && e["state"] == "closed" })
}

func TestRTCBridgeRejectsBadInput(t *testing.T) {
	events := newRTCEvents()
	bridge := &RTCBridge{Deliver: events.deliver}
	t.Cleanup(bridge.Shutdown)
	urls, _ := json.Marshal([]string{})
	bridge.Handle(BridgeMessage{Op: "rtc-new", PID: "p1", ICEServers: []RTCServer{{URLs: urls}}})
	if failed := events.wait(t, event("failed")); !strings.Contains(failed["error"].(string), "urls") {
		t.Fatalf("failed = %v", failed)
	}
	bridge.Handle(BridgeMessage{Op: "rtc-offer", PID: "nope", RID: "r1"})
	if reply := events.wait(t, result("r1")); reply["error"] == nil {
		t.Fatalf("a request for an unknown peer must fail: %v", reply)
	}
	bridge.Handle(BridgeMessage{Op: "rtc-new", PID: "p2"})
	bridge.Handle(BridgeMessage{Op: "rtc-remote", PID: "p2", RID: "r2", SDPType: "bogus", SDP: "v=0"})
	if reply := events.wait(t, result("r2")); reply["error"] == nil {
		t.Fatalf("an unknown description type must fail: %v", reply)
	}
}
