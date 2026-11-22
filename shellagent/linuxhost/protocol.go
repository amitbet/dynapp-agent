// Package linuxhost is the Linux app host: dynapp-app-host shows one
// installed DynApp in a GTK window with WebKitGTK (docs/native-host.md in the
// dynapp repository). The GTK code is in gtkhost.c and host.go, built only
// with the webkitgtk tag; this file holds the parts that need no GUI.
package linuxhost

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	frameText   byte = 1
	frameBinary byte = 2
)

// Options come from the .desktop entry the agent wrote.
type Options struct {
	StoreID string
	AppID   string
	Socket  string
	URL     string
	Origin  string
	Name    string
	Icon    string
	Files   []string
	Check   bool
	Version bool
}

// Version is stamped at release build time with -X.
var Version = "dev"

// ParseOptions reads the host command line:
//
//	dynapp-app-host --id owner/slug --app-id io.dynapp.app.owner.slug --socket <path>
//	  --url <start> --origin <origin> --name <name> --icon <png> [--] [files…]
func ParseOptions(args []string) (Options, error) {
	var options Options
	set := flag.NewFlagSet("dynapp-app-host", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&options.StoreID, "id", "", "installed app store id (owner/slug)")
	set.StringVar(&options.AppID, "app-id", "", "application id")
	set.StringVar(&options.Socket, "socket", "", "agent socket")
	set.StringVar(&options.URL, "url", "", "start URL")
	set.StringVar(&options.Origin, "origin", "", "app origin")
	set.StringVar(&options.Name, "name", "", "app name")
	set.StringVar(&options.Icon, "icon", "", "window icon")
	set.BoolVar(&options.Check, "check", false, "check that the display opens, then exit")
	set.BoolVar(&options.Version, "version", false, "print the version, then exit")
	if err := set.Parse(args); err != nil {
		return options, err
	}
	options.Files = set.Args()
	if options.Check || options.Version {
		return options, nil
	}
	if options.StoreID == "" || options.Socket == "" || OriginKey(options.Origin) == "" {
		return options, errors.New("dynapp-app-host is started by DynApp shortcuts; it needs --id, --socket and --origin")
	}
	if OriginKey(options.URL) != OriginKey(options.Origin) {
		options.URL = options.Origin + "/?launch=pwa"
	}
	if options.Name == "" {
		options.Name = options.StoreID
	}
	return options, nil
}

// OriginKey is scheme://host[:port] in lower case, or "" for a URL without a
// host. Default ports are dropped, like a browser origin.
func OriginKey(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

func scheme(raw string) string {
	value, _, found := strings.Cut(raw, ":")
	if !found {
		return ""
	}
	return strings.ToLower(value)
}

// Verdict is a navigation decision for the GTK side.
type Verdict int

const (
	Allow  Verdict = 0
	Ignore Verdict = 1
)

// Policy decides navigation for one app origin. Pages of the app's origin
// load in the window; http(s), mailto and tel links elsewhere open in the
// user's browser, like the macOS and Windows hosts.
type Policy struct {
	Origin string
}

func (p Policy) isApp(raw string) bool { return OriginKey(raw) == p.Origin && p.Origin != "" }

// isDyner reports whether raw is on the catalog's registrable domain, whose
// sign-in pages may open in a popup of the app.
func (p Policy) isDyner(raw string) bool {
	host := strings.ToLower(hostOf(raw))
	app := strings.Split(strings.ToLower(hostOf(p.Origin)), ".")
	if host == "" || len(app) < 2 {
		return false
	}
	base := strings.Join(app[len(app)-2:], ".")
	return host == base || strings.HasSuffix(host, "."+base)
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func external(raw string) bool {
	switch scheme(raw) {
	case "http", "https", "mailto", "tel":
		return true
	}
	return false
}

// Navigation decides a navigation action. WebKitGTK also asks for iframe
// loads here without saying which frame, so loads that are not user clicks
// proceed and MainFrameResponse stops a main frame that leaves the app.
// It returns the URL to open in the browser, if any.
func (p Policy) Navigation(raw string, mainView, userGesture bool) (Verdict, string) {
	switch scheme(raw) {
	case "about", "blob", "data":
		return Allow, ""
	case "mailto", "tel":
		return Ignore, raw
	case "http", "https":
	default:
		return Ignore, ""
	}
	if !mainView || p.isApp(raw) || !userGesture {
		return Allow, ""
	}
	return Ignore, raw
}

// MainFrameResponse decides the main frame's document response.
func (p Policy) MainFrameResponse(raw string) (Verdict, string) {
	switch scheme(raw) {
	case "about", "blob", "data":
		return Allow, ""
	}
	if p.isApp(raw) {
		return Allow, ""
	}
	if external(raw) {
		return Ignore, raw
	}
	return Ignore, ""
}

// NewWindow decides window.open and target=_blank. Only the app's own origin
// and Dyner sign-in pages open in a popup; everything else goes to the
// browser.
func (p Policy) NewWindow(raw string) (Verdict, string) {
	if raw == "" || scheme(raw) == "about" || p.isApp(raw) || p.isDyner(raw) {
		return Allow, ""
	}
	if external(raw) {
		return Ignore, raw
	}
	return Ignore, ""
}

// BridgeMessage is one page → host message (docs/native-host.md).
type BridgeMessage struct {
	Marker    int               `json:"dynappNativeHost"`
	HostToken string            `json:"hostToken"`
	Op        string            `json:"op"`
	ID        string            `json:"id"`
	JSON      string            `json:"json"`
	Entries   []json.RawMessage `json:"entries"`
	SID       string            `json:"sid"`
	RID       string            `json:"rid"`
	Text      string            `json:"text"`
	Binary    string            `json:"binary"`
	Code      int               `json:"code"`
	Reason    string            `json:"reason"`
	Enabled   bool              `json:"enabled"`
	// WebTransport stand-in (webtransport.go).
	WID    string   `json:"wid"`
	Stream string   `json:"stream"`
	URL    string   `json:"url"`
	Hashes []string `json:"hashes"`
	// RTCPeerConnection stand-in (webrtc.go).
	PID            string          `json:"pid"`
	CID            string          `json:"cid"`
	Label          string          `json:"label"`
	Ordered        *bool           `json:"ordered"`
	MaxRetransmits *int            `json:"maxRetransmits"`
	ICEServers     []RTCServer     `json:"iceServers"`
	SDP            string          `json:"sdp"`
	SDPType        string          `json:"sdpType"`
	Candidate      json.RawMessage `json:"candidate"`
	IsBinary       bool            `json:"isBinary"`
	// Size is a send's byte count, or a channel's bufferedAmountLowThreshold.
	Size int `json:"size"`
}

// ParseBridgeMessage accepts a message only with the bridge marker, the
// launch token, and a sender document of the app's origin.
func ParseBridgeMessage(raw, documentURL, origin, token string) (BridgeMessage, error) {
	var message BridgeMessage
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		return message, err
	}
	if message.Marker != 1 {
		return message, errors.New("not a bridge message")
	}
	if token == "" || message.HostToken != token {
		return message, errors.New("bridge message without the launch token")
	}
	if OriginKey(documentURL) != origin || origin == "" {
		return message, fmt.Errorf("bridge message from %q", documentURL)
	}
	return message, nil
}

// WrapBootstrap scopes the per-launch token to the agent's bridge script and
// the host's WebTransport stand-in, so only those closures can sign messages
// to the host.
func WrapBootstrap(bootstrap, token string) string {
	encoded, _ := json.Marshal(token)
	return "(() => {\nconst __dynappHostToken = " + string(encoded) + ";\n" + bootstrap + "\n" + webTransportPolyfill + "\n" + webRTCPolyfill + "\n})();"
}

// PromptAnswer is what the permission page posted.
type PromptAnswer struct {
	Resize       float64  `json:"resize"`
	Decision     string   `json:"decision"`
	Capabilities []string `json:"capabilities"`
	DontAsk      bool     `json:"dontAsk"`
}

// ParsePromptAnswer returns the page message; ok is false for anything that
// is neither a resize nor a decision.
func ParsePromptAnswer(raw string) (PromptAnswer, bool) {
	var answer PromptAnswer
	if json.Unmarshal([]byte(raw), &answer) != nil {
		return answer, false
	}
	switch answer.Decision {
	case "allow", "deny", "dismissed":
		if answer.Decision != "allow" {
			answer.Capabilities = nil
		} else if answer.Capabilities == nil {
			answer.Capabilities = []string{}
		}
		return answer, true
	case "":
		return answer, answer.Resize > 0
	}
	return answer, false
}

// FrameConn speaks the agent's native channel framing:
// uint32 big-endian length | uint8 kind | payload.
type FrameConn struct {
	Conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
}

func NewFrameConn(conn net.Conn) *FrameConn {
	return &FrameConn{Conn: conn, reader: bufio.NewReaderSize(conn, 64*1024)}
}

func DialAgent(socket string) (*FrameConn, error) {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return NewFrameConn(conn), nil
}

func (c *FrameConn) Read() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:4])
	if length < 1 || length > 64<<20 {
		return 0, nil, errors.New("invalid agent frame")
	}
	payload := make([]byte, length-1)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, nil, err
	}
	return header[4], payload, nil
}

func (c *FrameConn) Write(kind byte, payload []byte) error {
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)+1))
	frame[4] = kind
	copy(frame[5:], payload)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.Conn.Write(frame)
	return err
}

func (c *FrameConn) WriteJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.Write(frameText, data)
}

func (c *FrameConn) Close() error { return c.Conn.Close() }
