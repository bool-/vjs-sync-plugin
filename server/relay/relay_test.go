package relay

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bool-/vjs-sync-plugin/server/plex"
	"github.com/bool-/vjs-sync-plugin/server/plex/plextest"
)

type fakeDir struct {
	mu    sync.Mutex
	rooms map[string]Media
	conns map[uint64]string
}

func (d *fakeDir) Lookup(room string, conn uint64) (Media, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conns[conn] != room {
		return Media{}, false
	}
	m, ok := d.rooms[room]
	return m, ok
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	t     *testing.T
	fake  *plextest.Server
	relay *Relay
	dir   *fakeDir
	clock *clock
	srv   *httptest.Server
	logs  *strings.Builder
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fake := plextest.New()
	pc, err := plex.New(fake.URL, plextest.Token, fake.Client())
	if err != nil {
		t.Fatal(err)
	}
	levels, _ := plex.ParseLevels("480:1500,720:4000,1080:8000")
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	dir := &fakeDir{rooms: map[string]Media{}, conns: map[uint64]string{}}
	logs := &strings.Builder{}
	var logMu sync.Mutex
	r, err := New(Config{Plex: pc, Levels: levels, Dir: dir, MaxSessions: 6, CacheBytes: 1 << 20,
		Logger: log.New(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logs.Write(p) }), "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	rg := &rig{t: t, fake: fake, relay: r, dir: dir, clock: clk, srv: srv, logs: logs}
	t.Cleanup(func() {
		srv.Close()
		fake.Close()
		for _, p := range fake.Problems() {
			t.Errorf("fake Plex saw: %s", p)
		}
		logMu.Lock()
		if strings.Contains(logs.String(), plextest.Token) {
			t.Errorf("token in relay logs: %s", logs.String())
		}
		logMu.Unlock()
	})
	return rg
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (rg *rig) show(room string, conn uint64, key string, gen uint64) string {
	rg.t.Helper()
	it, err := rg.relay.cfg.Plex.Metadata(context.Background(), key)
	if err != nil {
		rg.t.Fatal(err)
	}
	rg.dir.mu.Lock()
	rg.dir.rooms[room] = Media{Gen: gen, Item: it}
	rg.dir.conns[conn] = room
	rg.dir.mu.Unlock()
	return rg.relay.Grant(room, conn)
}

type resp struct {
	code int
	hdr  http.Header
	body string
}

func (rg *rig) get(path string) resp {
	rg.t.Helper()
	res, err := http.Get(rg.srv.URL + path)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := resp{res.StatusCode, res.Header, string(b)}
	rg.noLeak(path, r)
	return r
}

// noLeak is the central check: the token never appears in anything a
// viewer receives, and no Plex header, cookie or redirect is passed on.
func (rg *rig) noLeak(path string, r resp) {
	rg.t.Helper()
	if strings.Contains(r.body, plextest.Token) {
		rg.t.Fatalf("%s: token in body", path)
	}
	for k, vs := range r.hdr {
		for _, v := range vs {
			if strings.Contains(v, plextest.Token) {
				rg.t.Fatalf("%s: token in header %s", path, k)
			}
		}
		if lk := strings.ToLower(k); lk == "set-cookie" || lk == "location" || strings.HasPrefix(lk, "x-plex") {
			rg.t.Fatalf("%s: upstream header %s passed through", path, k)
		}
	}
	if strings.Contains(r.body, "plex.direct") || strings.Contains(r.body, "127.0.0.1") || strings.Contains(r.body, "/video/:/") {
		rg.t.Fatalf("%s: upstream address in body", path)
	}
}

func TestPlaybackPathsNeverLeak(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "100", 1)

	m := rg.get(base + "1/master.m3u8")
	if m.code != 200 || !strings.Contains(m.body, "1080/index.m3u8") || m.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("master: %d %q", m.code, m.body)
	}
	idx := rg.get(base + "1/720/index.m3u8")
	if idx.code != 200 || !strings.Contains(idx.body, "#EXT-X-ENDLIST") || !strings.Contains(idx.body, "\n119.ts\n") {
		t.Fatalf("index: %d %q", idx.code, idx.body)
	}
	seg := rg.get(base + "1/720/7.ts")
	if seg.code != 200 || seg.hdr.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("segment: %d %v", seg.code, seg.hdr)
	}
	p := rg.get(base + "1/poster.jpg")
	if p.code != 200 || p.hdr.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("poster: %d", p.code)
	}

	// Every error path: upstream 500, redirect, timeout-ish, oversize.
	rg.fake.FailNext["/session/"] = 500
	rg.get(base + "1/720/8.ts")
	rg.fake.FailNext["/session/"] = 302
	rg.get(base + "1/720/9.ts")
	rg.fake.Oversized = true
	if r := rg.get(base + "1/720/10.ts"); r.code != 502 || r.body != "" {
		t.Fatalf("oversized: %d %q", r.code, r.body)
	}
}

func TestSegmentBodiesAreOnlyFromOurSession(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "100", 1)
	res, _ := http.Get(rg.srv.URL + base + "1/480/3.ts")
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "seg ms-party-k100-480-") || !strings.Contains(string(b), " 3 ") {
		t.Fatalf("segment came from the wrong place: %q", string(b[100:160]))
	}
}

func TestRefusals(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "102", 1) // a 720p item: no 1080 level
	other := rg.show("other", 2, "100", 1)
	g := strings.TrimSuffix(strings.TrimPrefix(base, "/media/"), "/")

	cases := map[string]string{
		"no 1080 for a 720p item":   base + "1/1080/index.m3u8",
		"level not configured":      base + "1/360/index.m3u8",
		"stale generation":          base + "2/master.m3u8",
		"leading zero":              base + "01/master.m3u8",
		"seq leading zero":          base + "1/720/007.ts",
		"seq past the end":          base + "1/720/99999.ts",
		"traversal":                 base + "1/../../../identity",
		"encoded traversal":         base + "1/%2e%2e/master.m3u8",
		"extra segment":             base + "1/720/1/2.ts",
		"wrong suffix":              base + "1/720/1.mp4",
		"forged grant":              "/media/" + g[:len(g)-3] + "AAA/1/master.m3u8",
		"no grant":                  "/media/1/master.m3u8",
		"another room's grant":      strings.Replace(other, "/media/", "/media/", 1) + "2/master.m3u8",
		"query string":              base + "1/master.m3u8?X-Plex-Token=x",
		"unknown file":              base + "1/index.m3u8",
		"plex path":                 "/video/:/transcode/universal/start.m3u8",
	}
	for name, path := range cases {
		r := rg.get(path)
		if r.code != 404 || r.body != "" {
			t.Errorf("%s: %s -> %d %q", name, path, r.code, r.body)
		}
	}

	// A connection that left the room loses access at once.
	rg.dir.mu.Lock()
	delete(rg.dir.conns, 1)
	rg.dir.mu.Unlock()
	if r := rg.get(base + "1/master.m3u8"); r.code != 404 {
		t.Fatalf("departed connection: %d", r.code)
	}

	// Expired grants.
	rg2 := newRig(t)
	b2 := rg2.show("party", 1, "100", 1)
	rg2.clock.Add(grantTTL + time.Second)
	if r := rg2.get(b2 + "1/master.m3u8"); r.code != 404 {
		t.Fatalf("expired grant: %d", r.code)
	}

	res, _ := http.Post(rg.srv.URL+other+"1/master.m3u8", "text/plain", nil)
	if res.StatusCode != 405 {
		t.Fatalf("POST: %d", res.StatusCode)
	}
}

func TestOneUpstreamFetchPerSegment(t *testing.T) {
	rg := newRig(t)
	rg.fake.SegDelay = 150 * time.Millisecond
	base := rg.show("party", 1, "100", 1)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := http.Get(rg.srv.URL + base + "1/720/20.ts")
			if err == nil && res.StatusCode == 200 {
				ok.Add(1)
			}
			if res != nil {
				res.Body.Close()
			}
		}()
	}
	wg.Wait()
	sessions := rg.fake.Started()
	if ok.Load() != 12 || len(sessions) != 1 {
		t.Fatalf("ok=%d sessions=%v", ok.Load(), sessions)
	}
	if n := rg.fake.Fetches(sessions[0], 20); n != 1 {
		t.Fatalf("segment fetched %d times from Plex, want 1", n)
	}
	rg.get(base + "1/720/20.ts")
	if n := rg.fake.Fetches(sessions[0], 20); n != 1 {
		t.Fatalf("cached segment refetched: %d", n)
	}
}

func TestSessionRestartsForEarlierSegments(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "100", 1)
	m, _ := rg.dir.Lookup("party", 1)
	rg.relay.Warm("party", m, 300, []int{720})
	waitFor(t, func() bool { return len(rg.fake.Started()) == 1 && rg.relay.Stats().CacheItems >= prefetchSegs })
	first := rg.fake.Started()[0]
	// Warm-up prefetched 60, 61, 62: these are hits.
	rg.get(base + "1/720/60.ts")
	if n := rg.fake.Fetches(first, 60); n != 1 {
		t.Fatalf("prefetched segment fetched %d times", n)
	}
	// Segment 10 is before the session's start: the relay restarts at it.
	if r := rg.get(base + "1/720/10.ts"); r.code != 200 {
		t.Fatalf("earlier segment: %d", r.code)
	}
	started := rg.fake.Started()
	if len(started) != 2 {
		t.Fatalf("want a restart, sessions started: %v", started)
	}
	waitFor(t, func() bool { return len(rg.fake.Stopped()) == 1 && rg.fake.Stopped()[0] == first })
}

func TestLevelsShareNothingButTheRoom(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "100", 1)
	for _, l := range []string{"480", "720", "1080"} {
		if r := rg.get(base + "1/" + l + "/0.ts"); r.code != 200 {
			t.Fatalf("level %s: %d", l, r.code)
		}
	}
	if n := len(rg.fake.Started()); n != 3 {
		t.Fatalf("want one session per level, got %d", n)
	}
	if rg.relay.Stats().Sessions != 3 {
		t.Fatalf("stats: %+v", rg.relay.Stats())
	}
}

func TestSessionCap(t *testing.T) {
	rg := newRig(t)
	rg.relay.cfg.MaxSessions = 2
	base := rg.show("party", 1, "100", 1)
	rg.get(base + "1/480/0.ts")
	rg.get(base + "1/720/0.ts")
	if r := rg.get(base + "1/1080/0.ts"); r.code != 503 {
		t.Fatalf("third session past the cap: %d", r.code)
	}
}

func TestIdleSessionsStopAndOrphansAreSwept(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "100", 1)
	rg.get(base + "1/720/0.ts")
	id := rg.fake.Started()[0]
	rg.clock.Add(30 * time.Second)
	rg.relay.upkeep()
	if len(rg.fake.Stopped()) != 0 {
		t.Fatal("stopped while still in use")
	}
	rg.clock.Add(31 * time.Second)
	rg.relay.upkeep()
	waitFor(t, func() bool { return len(rg.fake.Stopped()) == 1 && rg.fake.Stopped()[0] == id })

	// An orphan from a previous run: ours by prefix, not tracked.
	pc := rg.relay.cfg.Plex
	if _, err := pc.StartTranscode(context.Background(), plex.TranscodeParams{Key: "100", Session: "ms-party-k100-720-dead00", Width: 1280, Height: 720, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	// And someone else's session, which must be left alone.
	if _, err := pc.StartTranscode(context.Background(), plex.TranscodeParams{Key: "100", Session: "someone-elses-session", Width: 1280, Height: 720, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	rg.relay.sweep()
	running := strings.Join(rg.fake.Running(), ",")
	if strings.Contains(running, "dead00") || !strings.Contains(running, "someone-elses-session") {
		t.Fatalf("sweep left %s", running)
	}
}

func TestReleaseStopsOldGenerations(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "211", 1)
	rg.get(base + "1/720/0.ts")
	rg.show("party", 1, "212", 2)
	rg.relay.Release("party", 2)
	waitFor(t, func() bool { return len(rg.fake.Stopped()) == 1 })
	if b, _ := rg.relay.cache.size(); b != 0 {
		t.Fatalf("old generation still cached: %d bytes", b)
	}
	if r := rg.get(base + "1/720/1.ts"); r.code != 404 {
		t.Fatalf("old generation still served: %d", r.code)
	}
}

func TestRateLimit(t *testing.T) {
	rg := newRig(t)
	base := rg.show("party", 1, "100", 1)
	limited := 0
	for i := 0; i < rateBurst+20; i++ {
		if r := rg.get(base + "1/master.m3u8"); r.code == 429 {
			limited++
		}
	}
	if limited < 15 {
		t.Fatalf("only %d requests limited", limited)
	}
}

func TestGrantTampering(t *testing.T) {
	g, _ := NewGrants(time.Now)
	s := g.Issue("party", 7, time.Hour)
	if room, conn, ok := g.Check(s); !ok || room != "party" || conn != 7 {
		t.Fatal("valid grant refused")
	}
	payload, sig, _ := strings.Cut(s, ".")
	forged := b64.EncodeToString([]byte("other|7|9999999999")) + "." + sig
	for _, bad := range []string{forged, payload, payload + ".", "." + sig, s + "x", strings.ToUpper(s)} {
		if _, _, ok := g.Check(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	g2, _ := NewGrants(time.Now)
	if _, _, ok := g2.Check(s); ok {
		t.Fatal("grant from another process accepted")
	}
}

// FuzzParse feeds the path parser arbitrary input. Whatever it accepts must
// be one of the four shapes, spelled exactly, with no way to name anything
// on Plex.
func FuzzParse(f *testing.F) {
	g, _ := NewGrants(time.Now)
	good := g.Issue("party", 1, time.Hour)
	for _, s := range []string{
		"/media/" + good + "/1/master.m3u8", "/media/" + good + "/1/720/3.ts", "/media/" + good + "/1/720/index.m3u8",
		"/media/" + good + "/1/poster.jpg", "/media/" + good + "/../1/master.m3u8", "/media/" + good + "/1/720/%2e%2e",
		"/media//1/master.m3u8", "/media/" + good + "/1/720/3.ts\x00", "/media/" + good + "/1/７２０/3.ts",
		"/media/" + good + "/1/720/-3.ts", "/media/" + good + "/99999999999999999999/master.m3u8",
	} {
		f.Add(s)
	}
	levels, _ := plex.ParseLevels("480:1500,720:4000,1080:8000")
	r := &Relay{cfg: Config{Levels: levels}, grants: g}
	f.Fuzz(func(t *testing.T, path string) {
		q, ok := r.parse(path)
		if !ok {
			return
		}
		var want string
		switch q.kind {
		case "master":
			want = fmt.Sprintf("/media/%s/%d/master.m3u8", q.grant, q.gen)
		case "poster":
			want = fmt.Sprintf("/media/%s/%d/poster.jpg", q.grant, q.gen)
		case "index":
			want = fmt.Sprintf("/media/%s/%d/%d/index.m3u8", q.grant, q.gen, q.level)
		case "segment":
			want = fmt.Sprintf("/media/%s/%d/%d/%d.ts", q.grant, q.gen, q.level, q.seq)
		default:
			t.Fatalf("accepted unknown kind %q for %q", q.kind, path)
		}
		if path != want {
			t.Fatalf("accepted %q, which is not its canonical spelling %q", path, want)
		}
		if q.room != "party" || q.conn != 1 {
			t.Fatalf("grant decoded to %q/%d", q.room, q.conn)
		}
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
