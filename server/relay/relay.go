// Package relay serves a room's media to its viewers from one Plex
// transcode per quality level.
//
// Viewers can request exactly four kinds of path, all under a grant that
// ties them to a room and a connection:
//
//	/media/{grant}/{gen}/master.m3u8
//	/media/{grant}/{gen}/{level}/index.m3u8
//	/media/{grant}/{gen}/{level}/{seq}.ts
//	/media/{grant}/{gen}/poster.jpg
//
// Nothing a viewer sends becomes part of a Plex URL: upstream URLs come
// only from the relay's own sessions. Responses are written from scratch,
// never copied from Plex, and every error is an empty body with a status.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bool-/vjs-sync-plugin/server/plex"
)

// Media is what a room is showing.
type Media struct {
	Gen  uint64    // bumps whenever the room's media changes
	Item plex.Item // from the resolver, already checked against the library allowlist
}

// Directory is the relay's view of the rooms, provided by the sync server.
type Directory interface {
	// Lookup reports whether connection conn is still in room, and what the
	// room is showing. ok is false if either has gone.
	Lookup(room string, conn uint64) (m Media, ok bool)
}

// Config sets the relay up.
type Config struct {
	Plex        *plex.Client
	Levels      []plex.Level
	Dir         Directory
	MaxSessions int   // across all rooms
	CacheBytes  int64 // segment cache budget
	Logger      *log.Logger
	Now         func() time.Time
}

const (
	maxSegment      = 16 << 20
	idleStop        = 60 * time.Second
	pingEvery       = 20 * time.Second
	sweepEvery      = 5 * time.Minute
	sessionPrefix   = "ms-"
	prefetchSegs    = 3
	grantTTL        = 6 * time.Hour
	ratePerSecond   = 50
	rateBurst       = 100
	posterW, posterH = 400, 600
)

// Relay is the media relay.
type Relay struct {
	cfg    Config
	grants *Grants
	cache  *cache

	mu       sync.Mutex
	sessions map[sessionKey]*session
	limits   map[string]*bucket
}

type sessionKey struct {
	room  string
	gen   uint64
	level int
}

type session struct {
	id      string
	key     string
	offset  int
	ready   chan struct{} // closed when the start finishes
	err     error
	tr      *plex.Transcode
	segs    []plex.Segment
	first   int // first segment Plex produces for this session
	used    time.Time
	pinged  time.Time
	stopped bool
}

// New makes a relay.
func New(cfg Config) (*Relay, error) {
	if cfg.Plex == nil || cfg.Dir == nil || len(cfg.Levels) == 0 {
		return nil, errors.New("relay: Plex, Dir and Levels are required")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 6
	}
	if cfg.CacheBytes <= 0 {
		cfg.CacheBytes = 128 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	g, err := NewGrants(cfg.Now)
	if err != nil {
		return nil, err
	}
	return &Relay{
		cfg:      cfg,
		grants:   g,
		cache:    newCache(cfg.CacheBytes),
		sessions: map[sessionKey]*session{},
		limits:   map[string]*bucket{},
	}, nil
}

// Grant issues the media path prefix for one connection in one room.
func (r *Relay) Grant(room string, conn uint64) string {
	return "/media/" + r.grants.Issue(room, conn, grantTTL) + "/"
}

// Levels lists the quality levels an item offers: those at or below its
// own tier, and always at least the lowest.
func (r *Relay) Levels(it plex.Item) []int {
	var out []int
	for _, l := range r.cfg.Levels {
		if l.Height <= it.Height || len(out) == 0 {
			out = append(out, l.Height)
		}
	}
	return out
}

func (r *Relay) level(height int) (plex.Level, bool) {
	for _, l := range r.cfg.Levels {
		if l.Height == height {
			return l, true
		}
	}
	return plex.Level{}, false
}

// ---- requests ----

type request struct {
	room  string
	conn  uint64
	grant string
	gen   uint64
	level int    // 0 for master and poster
	kind  string // master, index, segment, poster
	seq   int
}

// parse matches the path against the four shapes exactly. It is
// deliberately written by hand: no cleaning, no prefix matching.
func (r *Relay) parse(path string) (request, bool) {
	var q request
	rest, ok := strings.CutPrefix(path, "/media/")
	if !ok {
		return q, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || len(parts) > 4 {
		return q, false
	}
	room, conn, ok := r.grants.Check(parts[0])
	if !ok {
		return q, false
	}
	q.room, q.conn, q.grant = room, conn, parts[0]
	gen, ok := digits(parts[1], 12)
	if !ok {
		return q, false
	}
	q.gen = uint64(gen)
	if len(parts) == 3 {
		switch parts[2] {
		case "master.m3u8":
			q.kind = "master"
		case "poster.jpg":
			q.kind = "poster"
		default:
			return q, false
		}
		return q, true
	}
	h, ok := digits(parts[2], 4)
	if !ok {
		return q, false
	}
	if _, ok := r.level(h); !ok {
		return q, false
	}
	q.level = h
	if parts[3] == "index.m3u8" {
		q.kind = "index"
		return q, true
	}
	name, ok := strings.CutSuffix(parts[3], ".ts")
	if !ok {
		return q, false
	}
	seq, ok := digits(name, 5)
	if !ok {
		return q, false
	}
	q.kind, q.seq = "segment", seq
	return q, true
}

// digits parses 1..max ASCII digits with no sign, space or leading zero
// (other than "0" itself), so every accepted path has one spelling.
func digits(s string, max int) (int, bool) {
	if s == "" || len(s) > max || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func requestID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// fail writes a status with an empty body and only our own headers.
func fail(w http.ResponseWriter, id string, code int) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("X-Request-Id", id)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", "0")
	w.WriteHeader(code)
}

func reply(w http.ResponseWriter, req *http.Request, id, contentType, cacheControl string, body []byte) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Request-Id", id)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if req.Method != http.MethodHead {
		w.Write(body)
	}
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id := requestID()
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		fail(w, id, http.StatusMethodNotAllowed)
		return
	}
	if req.URL.RawQuery != "" {
		fail(w, id, http.StatusNotFound)
		return
	}
	q, ok := r.parse(req.URL.EscapedPath())
	if !ok {
		fail(w, id, http.StatusNotFound)
		return
	}
	if !r.allow(q.grant) {
		fail(w, id, http.StatusTooManyRequests)
		return
	}
	m, ok := r.cfg.Dir.Lookup(q.room, q.conn)
	if !ok || m.Gen != q.gen || m.Item.Key == "" {
		fail(w, id, http.StatusNotFound)
		return
	}
	if q.level != 0 && !contains(r.Levels(m.Item), q.level) {
		fail(w, id, http.StatusNotFound)
		return
	}
	ctx := req.Context()
	switch q.kind {
	case "master":
		reply(w, req, id, "application/vnd.apple.mpegurl", "no-store", r.master(m.Item))
	case "index":
		s, err := r.session(ctx, q.room, m, q.level, -1, false)
		if err != nil {
			r.upstream(w, id, q, err)
			return
		}
		reply(w, req, id, "application/vnd.apple.mpegurl", "no-store", mediaPlaylist(s.segs))
	case "segment":
		b, err := r.segment(ctx, q.room, m, q.level, q.seq)
		if err != nil {
			r.upstream(w, id, q, err)
			return
		}
		reply(w, req, id, "video/mp2t", "private, max-age=3600", b)
	case "poster":
		b, err := r.poster(ctx, q.room, m)
		if err != nil {
			r.upstream(w, id, q, err)
			return
		}
		reply(w, req, id, "image/jpeg", "private, max-age=3600", b)
	}
}

var errNotFound = errors.New("not found")
var errBusy = errors.New("too many sessions")

// upstream logs the cause and answers with a bare status. The log line has
// the request ID, room and kind; plex errors are already free of queries.
func (r *Relay) upstream(w http.ResponseWriter, id string, q request, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, errNotFound):
		code = http.StatusNotFound
	case errors.Is(err, errBusy):
		code = http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled):
		return
	}
	r.cfg.Logger.Printf("relay %s room=%s %s level=%d seq=%d: %v", id, q.room, q.kind, q.level, q.seq, err)
	fail(w, id, code)
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ---- playlists, written from parsed data only ----

func (r *Relay) master(it plex.Item) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, h := range r.Levels(it) {
		l, _ := r.level(h)
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,NAME=\"%dp\"\n%d/index.m3u8\n", l.Kbps*1000, l.Width(), l.Height, l.Height, l.Height)
	}
	return []byte(b.String())
}

func mediaPlaylist(segs []plex.Segment) []byte {
	target := 1.0
	for _, s := range segs {
		target = math.Max(target, s.Duration)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", int(math.Ceil(target)))
	for i, s := range segs {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%d.ts\n", s.Duration, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// ---- sessions ----

func newSessionID(room, key string, level int) string {
	var b [3]byte
	rand.Read(b[:])
	id := fmt.Sprintf("%s%s-k%s-%d-%s", sessionPrefix, room, key, level, hex.EncodeToString(b[:]))
	if len(id) > 64 {
		id = fmt.Sprintf("%s%s-k%s-%d-%s", sessionPrefix, room[:8], key, level, hex.EncodeToString(b[:]))
	}
	return id
}

// session returns the running session for (room, gen, level), starting one
// if needed. offset < 0 means any running session will do (a new one then
// starts at 0). With restart, or when offset lies before where the running
// session started, it is replaced by one starting at offset.
func (r *Relay) session(ctx context.Context, room string, m Media, level, offset int, restart bool) (*session, error) {
	k := sessionKey{room, m.Gen, level}
	r.mu.Lock()
	s := r.sessions[k]
	if s != nil {
		select {
		case <-s.ready:
			if s.err != nil || restart || (offset >= 0 && s.offset > offset) {
				r.dropLocked(k, s)
				s = nil
			}
		default:
		}
	}
	if s == nil {
		if r.countLocked() >= r.cfg.MaxSessions {
			r.mu.Unlock()
			return nil, errBusy
		}
		l, ok := r.level(level)
		if !ok {
			r.mu.Unlock()
			return nil, errNotFound
		}
		s = &session{id: newSessionID(room, m.Item.Key, level), key: m.Item.Key, offset: max(offset, 0), ready: make(chan struct{}), used: r.cfg.Now(), pinged: r.cfg.Now()}
		r.sessions[k] = s
		r.mu.Unlock()
		go r.start(s, l)
		r.mu.Lock()
	}
	s.used = r.cfg.Now()
	r.mu.Unlock()
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

func (r *Relay) start(s *session, l plex.Level) {
	defer close(s.ready)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := r.cfg.Plex.StartTranscode(ctx, plex.TranscodeParams{
		Key: s.key, Session: s.id, Width: l.Width(), Height: l.Height, Kbps: l.Kbps, Offset: s.offset,
	})
	if err != nil {
		s.err = err
		return
	}
	segs, err := r.cfg.Plex.Playlist(ctx, tr)
	if err != nil {
		s.err = err
		go r.cfg.Plex.Stop(context.Background(), s.id)
		return
	}
	s.tr, s.segs = tr, segs
	for i, seg := range segs {
		if seg.Start+seg.Duration > float64(s.offset) {
			s.first = i
			break
		}
	}
}

func (r *Relay) countLocked() int {
	n := 0
	for _, s := range r.sessions {
		if !s.stopped {
			n++
		}
	}
	return n
}

// dropLocked forgets a session and stops it on Plex in the background.
func (r *Relay) dropLocked(k sessionKey, s *session) {
	delete(r.sessions, k)
	if !s.stopped {
		s.stopped = true
		go func() {
			<-s.ready
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := r.cfg.Plex.Stop(ctx, s.id); err != nil {
				// One retry; the sweep catches anything still left.
				time.Sleep(time.Second)
				r.cfg.Plex.Stop(ctx, s.id)
			}
		}()
	}
}

// ---- segments ----

func (r *Relay) segment(ctx context.Context, room string, m Media, level, seq int) ([]byte, error) {
	ck := cacheKey{room: room, gen: m.Gen, level: level, seq: seq}
	return r.cache.get(ctx, ck, func() ([]byte, error) {
		return r.fetch(room, m, level, seq)
	})
}

// fetch gets a segment from Plex. If it lies before what the session
// produces, the session restarts there. An empty answer gets one retry.
func (r *Relay) fetch(room string, m Media, level, seq int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := r.session(ctx, room, m, level, -1, false)
	if err != nil {
		return nil, err
	}
	if seq >= len(s.segs) {
		return nil, errNotFound
	}
	if seq < s.first {
		s, err = r.session(ctx, room, m, level, int(s.segs[seq].Start), true)
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		b, err := r.cfg.Plex.FetchSegment(ctx, s.tr, s.segs[seq], maxSegment)
		if err == nil || attempt == 1 || !errors.Is(err, plex.ErrEmptySegment) {
			return b, err
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Warm starts the given levels of a room's media at offset seconds and
// fetches the first few segments there, so the room's first requests are
// cache hits. It returns at once; the work runs in the background.
func (r *Relay) Warm(room string, m Media, offset float64, levels []int) {
	for _, level := range levels {
		if !contains(r.Levels(m.Item), level) {
			continue
		}
		go func(level int) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s, err := r.session(ctx, room, m, level, int(math.Max(0, offset)), false)
			if err != nil {
				r.cfg.Logger.Printf("relay warm room=%s level=%d: %v", room, level, err)
				return
			}
			first := s.first
			for i, seg := range s.segs {
				if seg.Start+seg.Duration > offset {
					first = i
					break
				}
			}
			for seq := first; seq < first+prefetchSegs && seq < len(s.segs); seq++ {
				if _, err := r.segment(ctx, room, m, level, seq); err != nil {
					r.cfg.Logger.Printf("relay prefetch room=%s level=%d seq=%d: %v", room, level, seq, err)
					return
				}
			}
		}(level)
	}
}

// Release stops a room's sessions for every generation other than keep
// (pass 0 to stop them all) and drops their cached segments.
func (r *Relay) Release(room string, keep uint64) {
	r.mu.Lock()
	for k, s := range r.sessions {
		if k.room == room && k.gen != keep {
			r.dropLocked(k, s)
		}
	}
	r.mu.Unlock()
	r.cache.dropRoom(room, keep)
}

// ---- posters ----

func (r *Relay) poster(ctx context.Context, room string, m Media) ([]byte, error) {
	if !m.Item.HasThumb() {
		return nil, errNotFound
	}
	ck := cacheKey{room: room, gen: m.Gen, level: 0, seq: -1}
	return r.cache.get(ctx, ck, func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return r.cfg.Plex.Poster(ctx, m.Item, posterW, posterH)
	})
}

// ---- upkeep ----

// Run keeps sessions alive while used, stops idle ones, and removes
// orphans left on Plex by this relay (after a crash, say). It returns when
// ctx ends, stopping every session first.
func (r *Relay) Run(ctx context.Context) {
	r.sweep()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastSweep := r.cfg.Now()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for k, s := range r.sessions {
				r.dropLocked(k, s)
			}
			r.mu.Unlock()
			return
		case <-tick.C:
		}
		r.upkeep()
		if r.cfg.Now().Sub(lastSweep) >= sweepEvery {
			r.sweep()
			lastSweep = r.cfg.Now()
		}
	}
}

func (r *Relay) upkeep() {
	now := r.cfg.Now()
	var ping []string
	r.mu.Lock()
	for k, s := range r.sessions {
		select {
		case <-s.ready:
		default:
			continue
		}
		switch {
		case now.Sub(s.used) >= idleStop:
			r.dropLocked(k, s)
		case now.Sub(s.pinged) >= pingEvery && s.err == nil:
			s.pinged = now
			ping = append(ping, s.id)
		}
	}
	for g, b := range r.limits {
		if now.Sub(b.at) > time.Minute {
			delete(r.limits, g)
		}
	}
	r.mu.Unlock()
	for _, id := range ping {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.cfg.Plex.Ping(ctx, id); err != nil {
			r.cfg.Logger.Printf("relay ping %s: %v", id, err)
		}
		cancel()
	}
}

// sweep stops Plex sessions this relay started but no longer tracks.
func (r *Relay) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	keys, err := r.cfg.Plex.Sessions(ctx)
	if err != nil {
		r.cfg.Logger.Printf("relay sweep: %v", err)
		return
	}
	r.mu.Lock()
	live := map[string]bool{}
	for _, s := range r.sessions {
		live[s.id] = true
	}
	r.mu.Unlock()
	for _, k := range keys {
		if strings.HasPrefix(k, sessionPrefix) && !live[k] && plex.ValidSession(k) {
			r.cfg.Logger.Printf("relay sweep: stopping orphan %s", k)
			r.cfg.Plex.Stop(ctx, k)
		}
	}
}

// Stats is a snapshot for /healthz. It never includes the token or a URL.
type Stats struct {
	Sessions   int   `json:"sessions"`
	CacheBytes int64 `json:"cacheBytes"`
	CacheItems int   `json:"cacheItems"`
}

func (r *Relay) Stats() Stats {
	r.mu.Lock()
	n := r.countLocked()
	r.mu.Unlock()
	b, items := r.cache.size()
	return Stats{Sessions: n, CacheBytes: b, CacheItems: items}
}

// ---- rate limiting ----

type bucket struct {
	tokens float64
	at     time.Time
}

func (r *Relay) allow(grant string) bool {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.limits[grant]
	if b == nil {
		b = &bucket{tokens: rateBurst, at: now}
		r.limits[grant] = b
	}
	b.tokens = math.Min(rateBurst, b.tokens+now.Sub(b.at).Seconds()*ratePerSecond)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
