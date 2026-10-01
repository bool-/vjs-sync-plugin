package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testClient is a bare WebSocket client: just enough to drive the server.
type testClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dial(t *testing.T, srv *httptest.Server, origin string) (*testClient, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	rand.Read(key)
	req := "GET /ws HTTP/1.1\r\nHost: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	if _, err := conn.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testClient{t: t, conn: conn, r: r}, resp
}

func (c *testClient) frame(fin bool, op byte, payload []byte) {
	c.t.Helper()
	b := byte(op)
	if fin {
		b |= 0x80
	}
	frame := []byte{b}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = binary.BigEndian.AppendUint16(append(frame, 0x80|126), uint16(n))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 0x80|127), uint64(n))
	}
	mask := []byte{1, 2, 3, 4}
	frame = append(frame, mask...)
	for i, p := range payload {
		frame = append(frame, p^mask[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	c.frame(true, opText, b)
}

// next reads one server frame.
func (c *testClient) next() (byte, []byte) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if head[1]&0x80 != 0 {
		c.t.Fatal("server frames must not be masked")
	}
	n := int(head[1] & 0x7F)
	if n == 126 {
		var ext [2]byte
		io.ReadFull(c.r, ext[:])
		n = int(binary.BigEndian.Uint16(ext[:]))
	}
	payload := make([]byte, n)
	io.ReadFull(c.r, payload)
	return head[0] & 0x0F, payload
}

// read returns the next JSON message of the given type, skipping others.
func (c *testClient) read(kind string) map[string]any {
	c.t.Helper()
	for {
		op, payload := c.next()
		if op != opText {
			c.t.Fatalf("expected text frame, got op %d", op)
		}
		var m map[string]any
		json.Unmarshal(payload, &m)
		if m["type"] == kind {
			return m
		}
	}
}

func newTestServer(t *testing.T) (*httptest.Server, *Hub) {
	hub := newHub(&Source{Src: "movie.mp4"}, 3)
	mux := http.NewServeMux()
	mux.Handle("/ws", serveWS(hub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hub
}

func TestHandshakeAcceptKey(t *testing.T) {
	// The example from RFC 6455 section 1.3.
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("acceptKey = %s", got)
	}
}

func TestPingPong(t *testing.T) {
	srv, _ := newTestServer(t)
	c, resp := dial(t, srv, "")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	before := nowMs()
	c.send(map[string]any{"type": "ping", "t0": 12345.5})
	m := c.read("pong")
	if m["t0"] != 12345.5 {
		t.Fatalf("t0 echoed as %v", m["t0"])
	}
	if ts := m["t"].(float64); ts < before || ts > nowMs() {
		t.Fatalf("server time %v outside [%v, now]", ts, before)
	}
}

func TestControlPingAnswered(t *testing.T) {
	srv, _ := newTestServer(t)
	c, _ := dial(t, srv, "")
	c.frame(true, opPing, []byte("hi"))
	op, payload := c.next()
	if op != opPong || string(payload) != "hi" {
		t.Fatalf("got op %d %q", op, payload)
	}
}

func TestCrossOriginRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	_, resp := dial(t, srv, "https://evil.example")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	_, resp = dial(t, srv, srv.URL)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("same origin refused: %d", resp.StatusCode)
	}
}

func TestCommandBroadcastsToRoom(t *testing.T) {
	srv, _ := newTestServer(t)
	a, _ := dial(t, srv, "")
	b, _ := dial(t, srv, "")
	other, _ := dial(t, srv, "")
	a.send(map[string]any{"type": "join", "room": "party"})
	b.send(map[string]any{"type": "join", "room": "party"})
	other.send(map[string]any{"type": "join", "room": "elsewhere"})
	if s := a.read("state"); s["paused"] != true || s["position"] != 0.0 || s["src"].(map[string]any)["src"] != "movie.mp4" {
		t.Fatalf("initial state %v", s)
	}
	b.read("state")
	other.read("state")

	a.send(map[string]any{"type": "command", "action": "play", "position": 42.0})
	for _, c := range []*testClient{a, b} {
		s := c.read("state")
		if s["paused"] != false || s["position"] != 42.0 || s["rev"] != 1.0 {
			t.Fatalf("after play: %v", s)
		}
	}

	// The other room heard nothing: its next message is our pong.
	other.send(map[string]any{"type": "ping", "t0": 1})
	op, payload := other.next()
	if op != opText || !strings.Contains(string(payload), `"pong"`) {
		t.Fatalf("other room got %s", payload)
	}
}

func TestFragmentedMessage(t *testing.T) {
	srv, _ := newTestServer(t)
	c, _ := dial(t, srv, "")
	msg := []byte(`{"type":"ping","t0":7}`)
	c.frame(false, opText, msg[:5])
	c.frame(true, opPing, nil) // control frames may interleave
	c.frame(false, opContinuation, msg[5:10])
	c.frame(true, opContinuation, msg[10:])
	if op, _ := c.next(); op != opPong {
		t.Fatalf("interleaved ping not answered, got op %d", op)
	}
	if m := c.read("pong"); m["t0"] != 7.0 {
		t.Fatalf("pong %v", m)
	}
}

func TestOversizedMessageCloses(t *testing.T) {
	srv, _ := newTestServer(t)
	c, _ := dial(t, srv, "")
	c.frame(true, opText, make([]byte, maxMessage+1))
	op, payload := c.next()
	if op != opClose || binary.BigEndian.Uint16(payload) != 1009 {
		t.Fatalf("got op %d %v, want close 1009", op, payload)
	}
}

func TestUnmaskedFrameCloses(t *testing.T) {
	srv, _ := newTestServer(t)
	c, _ := dial(t, srv, "")
	c.conn.Write([]byte{0x81, 0x02, 'h', 'i'})
	if op, payload := c.next(); op != opClose || binary.BigEndian.Uint16(payload) != 1002 {
		t.Fatalf("got op %d %v, want close 1002", op, payload)
	}
}

func TestLockedRoomRefusesCommands(t *testing.T) {
	srv, hub := newTestServer(t)
	room := hub.addScheduled("drive-in", hub.src)
	room.mu.Lock()
	room.schedule(nowMs() + 60000)
	room.mu.Unlock()

	c, _ := dial(t, srv, "")
	c.send(map[string]any{"type": "join", "room": "drive-in"})
	s := c.read("state")
	if s["locked"] != true || s["paused"] != false {
		t.Fatalf("state %v", s)
	}
	c.send(map[string]any{"type": "command", "action": "pause", "position": 1.0})
	c.read("error")
	room.mu.Lock()
	defer room.mu.Unlock()
	if room.paused || room.rev != 1 {
		t.Fatal("locked room changed")
	}
}

func TestBadRoomAndFullServer(t *testing.T) {
	srv, _ := newTestServer(t) // max 3 rooms
	c, _ := dial(t, srv, "")
	c.send(map[string]any{"type": "join", "room": "../etc"})
	c.read("error")
	for _, name := range []string{"a", "b", "c"} {
		x, _ := dial(t, srv, "")
		x.send(map[string]any{"type": "join", "room": name})
		x.read("state")
	}
	c.send(map[string]any{"type": "join", "room": "d"})
	c.read("error")
}

func TestEmptyPartyRoomIsRemoved(t *testing.T) {
	srv, hub := newTestServer(t)
	c, _ := dial(t, srv, "")
	c.send(map[string]any{"type": "join", "room": "party"})
	c.read("state")
	c.conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		n := len(hub.rooms)
		hub.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("room outlived its last viewer")
}

func TestApply(t *testing.T) {
	r := newRoom("x", nil, false)
	r.at = 1000
	pos := func(v float64) *float64 { return &v }

	if !r.apply("play", pos(10), 1000) || r.paused || r.position != 10 || !r.waiting {
		t.Fatalf("play waits for viewers: %+v", r)
	}
	if got := r.positionAt(3000); got != 10 {
		t.Fatalf("held at %v while waiting, want 10", got)
	}
	r.start(3000)
	if r.waiting || r.at != 3300 || r.rev != 2 {
		t.Fatalf("start: %+v", r)
	}
	if got := r.positionAt(5300); got != 12 {
		t.Fatalf("positionAt = %v, want 12", got)
	}
	// Play while playing at the same spot changes nothing.
	if r.apply("play", nil, 5300) || r.rev != 2 {
		t.Fatal("no-op play bumped rev")
	}
	if !r.apply("seek", pos(-5), 5300) || r.position != 0 || r.paused || !r.waiting {
		t.Fatalf("seek clamps to 0 and keeps playing: %+v", r)
	}
	if !r.apply("pause", pos(1e12), 6000) || r.position != maxPosition || !r.paused || r.waiting {
		t.Fatalf("pause clamps huge positions and ends the wait: %+v", r)
	}
	if r.apply("pause", pos(maxPosition), 9000) {
		t.Fatal("repeat pause at the same spot should be a no-op")
	}
	if r.apply("rewind", nil, 9000) {
		t.Fatal("unknown action applied")
	}
}

func TestShowFor(t *testing.T) {
	every, countdown := 75*time.Second, 20*time.Second
	anchor := 100000.0
	cases := []struct{ t, want float64 }{
		{anchor - 10000, anchor},           // before the first show: count down to it
		{anchor, anchor},                   // showtime
		{anchor + 54999, anchor},           // still the current showing
		{anchor + 55000, anchor + 75000},   // countdown to the next
		{anchor + 150000, anchor + 150000}, // a later showing
	}
	for _, tc := range cases {
		if got := showFor(tc.t, anchor, every, countdown); got != tc.want {
			t.Errorf("showFor(%v) = %v, want %v", tc.t, got, tc.want)
		}
	}
}

func TestRateLimit(t *testing.T) {
	c := &client{tokens: rateBurst, refilled: time.Unix(0, 0)}
	now := time.Unix(0, 0)
	allowed := 0
	for i := 0; i < 100; i++ {
		if c.allow(now) {
			allowed++
		}
	}
	if allowed != rateBurst {
		t.Fatalf("burst allowed %d, want %d", allowed, rateBurst)
	}
	if !c.allow(now.Add(100 * time.Millisecond)) {
		t.Fatal("budget did not refill")
	}
}

func TestRoomWaitsForEveryoneToBuffer(t *testing.T) {
	srv, _ := newTestServer(t)
	a, _ := dial(t, srv, "")
	b, _ := dial(t, srv, "")
	a.send(map[string]any{"type": "join", "room": "party"})
	a.read("state")
	b.send(map[string]any{"type": "join", "room": "party"})
	b.read("state")

	a.send(map[string]any{"type": "command", "action": "seek", "position": 30.0})
	a.read("state") // paused seek: no wait
	b.read("state")
	a.send(map[string]any{"type": "command", "action": "play"})
	s := a.read("state")
	b.read("state")
	if s["waiting"] != true || s["paused"] != false || s["position"] != 30.0 {
		t.Fatalf("play should wait: %v", s)
	}
	rev := s["rev"]

	a.send(map[string]any{"type": "ready", "rev": rev})
	b.send(map[string]any{"type": "ready", "rev": 999.0}) // stale: ignored
	b.send(map[string]any{"type": "ping", "t0": 1})
	if m := b.read("pong"); m == nil {
		t.Fatal("no pong")
	}
	before := nowMs()
	b.send(map[string]any{"type": "ready", "rev": rev})
	for _, c := range []*testClient{a, b} {
		s := c.read("state")
		at := s["at"].(float64)
		if s["waiting"] != false || s["rev"] != rev.(float64)+1 || at < before+250 || at > nowMs()+350 {
			t.Fatalf("start: %v (now %v)", s, nowMs())
		}
	}
}

func TestRoomStopsWaitingForStragglers(t *testing.T) {
	srv, hub := newTestServer(t)
	hub.maxWait = 200 * time.Millisecond
	a, _ := dial(t, srv, "")
	b, _ := dial(t, srv, "")
	a.send(map[string]any{"type": "join", "room": "party"})
	a.read("state")
	b.send(map[string]any{"type": "join", "room": "party"})
	b.read("state")

	a.send(map[string]any{"type": "command", "action": "play", "position": 5.0})
	rev := a.read("state")["rev"]
	a.send(map[string]any{"type": "ready", "rev": rev})
	// b never answers.
	if s := a.read("state"); s["waiting"] != false {
		t.Fatalf("still waiting after maxWait: %v", s)
	}
}

func TestRoomStartsWhenTheStragglerLeaves(t *testing.T) {
	srv, _ := newTestServer(t)
	a, _ := dial(t, srv, "")
	b, _ := dial(t, srv, "")
	a.send(map[string]any{"type": "join", "room": "party"})
	a.read("state")
	b.send(map[string]any{"type": "join", "room": "party"})
	b.read("state")

	a.send(map[string]any{"type": "command", "action": "play", "position": 5.0})
	rev := a.read("state")["rev"]
	a.send(map[string]any{"type": "ready", "rev": rev})
	b.conn.Close()
	if s := a.read("state"); s["waiting"] != false {
		t.Fatalf("%v", s)
	}
}
