// Package plextest is a hostile fake Plex server for tests. It behaves like
// the real one where it matters (a full-length on-demand playlist, empty
// segments before a session's start offset) and goes out of its way to leak
// the token: in playlist comments, error bodies, redirects, cookies, headers
// and metadata fields the relay must never forward. It also records any
// request that carries the token in its URL or goes to an unexpected path.
package plextest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token is the fake's token. Tests search every relay response for it.
const Token = "SECRETtok3n-DO-NOT-LEAK"

// ServerID is the fake's machineIdentifier.
const ServerID = "fake0000machine0000identifier00000000000"

// SegmentSeconds is the fake's segment length.
const SegmentSeconds = 5

// Item is a fake library entry.
type Item struct {
	Key, Type, Title, Show string
	Section, Season, Ep    int
	Seconds, Height        int
	Parent                 string
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	items     map[string]Item
	sessions  map[string]int // session -> first segment produced
	stopped   []string
	started   []string
	fetched   map[string]int // session/segment fetch counts
	problems  []string
	Prefs     map[string]any
	Version   string
	SegDelay  time.Duration
	FailNext  map[string]int // path substring -> status to answer once
	Oversized bool
}

// New starts a fake with a small library: two movies (one in a 4K
// section), a show with a season of four episodes.
func New() *Server {
	s := &Server{
		items: map[string]Item{
			"100": {Key: "100", Type: "movie", Title: "Charade", Section: 1, Seconds: 600, Height: 1080},
			"101": {Key: "101", Type: "movie", Title: "Charade 4K", Section: 6, Seconds: 600, Height: 2160},
			"102": {Key: "102", Type: "movie", Title: "Small Movie", Section: 1, Seconds: 300, Height: 720},
			"200": {Key: "200", Type: "show", Title: "The Show", Section: 2},
			"210": {Key: "210", Type: "season", Title: "Season 1", Show: "The Show", Section: 2, Season: 1, Parent: "200"},
			"211": {Key: "211", Type: "episode", Title: "Pilot", Show: "The Show", Section: 2, Season: 1, Ep: 1, Seconds: 120, Height: 1080, Parent: "210"},
			"212": {Key: "212", Type: "episode", Title: "Two", Show: "The Show", Section: 2, Season: 1, Ep: 2, Seconds: 120, Height: 1080, Parent: "210"},
			"213": {Key: "213", Type: "episode", Title: "Three", Show: "The Show", Section: 2, Season: 1, Ep: 3, Seconds: 120, Height: 1080, Parent: "210"},
			"214": {Key: "214", Type: "episode", Title: "Four", Show: "The Show", Section: 2, Season: 1, Ep: 4, Seconds: 120, Height: 1080, Parent: "210"},
		},
		sessions: map[string]int{},
		fetched:  map[string]int{},
		Prefs:    map[string]any{"WanPerStreamMaxUploadRate": 0, "WanTotalMaxUploadRate": 0, "WanPerUserStreamCount": 0},
		Version:  "1.43.4.10903-e5521bd8c",
		FailNext: map[string]int{},
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) problem(f string, a ...any) {
	s.mu.Lock()
	s.problems = append(s.problems, fmt.Sprintf(f, a...))
	s.mu.Unlock()
}

// Problems lists protocol violations by the client under test.
func (s *Server) Problems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.problems...)
}

// Started and Stopped list session IDs in order.
func (s *Server) Started() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...)
}

func (s *Server) Stopped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stopped...)
}

// Running lists sessions not yet stopped.
func (s *Server) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.sessions {
		out = append(out, k)
	}
	return out
}

// Fetches counts how many times a session's segment was fetched.
func (s *Server) Fetches(session string, seq int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetched[fmt.Sprintf("%s/%d", session, seq)]
}

var (
	reMeta     = regexp.MustCompile(`^/library/metadata/(\d+)$`)
	reChildren = regexp.MustCompile(`^/library/metadata/(\d+)/children$`)
	reMediaPL  = regexp.MustCompile(`^/video/:/transcode/universal/session/([a-z0-9-]+)/base/index\.m3u8$`)
	reSegment  = regexp.MustCompile(`^/video/:/transcode/universal/session/([a-z0-9-]+)/base/(\d{5})\.ts$`)
)

func (s *Server) leaky(w http.ResponseWriter) {
	w.Header().Set("Set-Cookie", "plex="+Token)
	w.Header().Set("X-Plex-Echo-Token", Token)
	w.Header().Set("X-Plex-Protocol", "1.0")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.RawQuery, Token) || strings.Contains(r.URL.Path, Token) {
		s.problem("token in URL: %s", r.URL.Path)
	}
	if r.Header.Get("X-Plex-Token") != Token && r.URL.Path != "/identity" {
		s.problem("missing token header: %s", r.URL.Path)
	}
	if r.URL.Query().Get("X-Plex-Token") != "" {
		s.problem("token query parameter: %s", r.URL.Path)
	}
	s.leaky(w)
	for sub, code := range s.FailNext {
		if strings.Contains(r.URL.Path, sub) {
			s.mu.Lock()
			delete(s.FailNext, sub)
			s.mu.Unlock()
			if code >= 300 && code < 400 {
				http.Redirect(w, r, "https://evil.example/?X-Plex-Token="+Token, code)
				return
			}
			http.Error(w, "error for token "+Token, code)
			return
		}
	}
	p := r.URL.Path
	switch {
	case p == "/identity":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"machineIdentifier": ServerID, "version": s.Version}})
	case p == "/library/sections":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Directory": []map[string]any{
			{"key": "1", "type": "movie", "title": "Movies"}, {"key": "2", "type": "show", "title": "TV Shows"},
			{"key": "6", "type": "movie", "title": "Movies - 4K"}}}})
	case p == "/:/prefs":
		var set []map[string]any
		for k, v := range s.Prefs {
			set = append(set, map[string]any{"id": k, "value": v})
		}
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Setting": set}})
	case reMeta.MatchString(p):
		it, ok := s.items[reMeta.FindStringSubmatch(p)[1]]
		if !ok {
			http.Error(w, "no such item "+Token, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Metadata": []any{s.raw(it)}}})
	case reChildren.MatchString(p):
		parent := reChildren.FindStringSubmatch(p)[1]
		var kids []any
		for _, k := range sortedKeys(s.items) {
			if s.items[k].Parent == parent {
				kids = append(kids, s.raw(s.items[k]))
			}
		}
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Metadata": kids}})
	case p == "/hubs/search":
		q := strings.ToLower(r.URL.Query().Get("query"))
		hubs := map[string][]any{}
		for _, k := range sortedKeys(s.items) {
			it := s.items[k]
			if strings.Contains(strings.ToLower(it.Title), q) && it.Type != "season" {
				hubs[it.Type] = append(hubs[it.Type], s.raw(it))
			}
		}
		var list []any
		for _, t := range []string{"movie", "show", "episode"} {
			list = append(list, map[string]any{"type": t, "Metadata": hubs[t]})
		}
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Hub": list}})
	case p == "/photo/:/transcode":
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("\xff\xd8\xff\xe0JFIF-fake-poster-" + Token))
	case p == "/video/:/transcode/universal/decision":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"generalDecisionText": "ok " + Token}})
	case p == "/video/:/transcode/universal/start.m3u8":
		q := r.URL.Query()
		sess := q.Get("session")
		it, ok := s.items[strings.TrimPrefix(q.Get("path"), "/library/metadata/")]
		if !ok || it.Seconds == 0 || r.Header.Get("X-Plex-Session-Identifier") != sess {
			http.Error(w, "bad start "+Token, http.StatusBadRequest)
			return
		}
		off, _ := strconv.Atoi(q.Get("offset"))
		s.mu.Lock()
		s.sessions[sess] = off / SegmentSeconds
		s.started = append(s.started, sess)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprintf(w, "#EXTM3U\n# token %s\n#EXT-X-STREAM-INF:BANDWIDTH=1\nsession/%s/base/index.m3u8\n", Token, sess)
	case reMediaPL.MatchString(p):
		sess := reMediaPL.FindStringSubmatch(p)[1]
		it, ok := s.itemForSession(sess)
		if !ok {
			http.Error(w, "no session "+Token, http.StatusNotFound)
			return
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n# %s\n", SegmentSeconds, Token)
		for i := 0; i*SegmentSeconds < it.Seconds; i++ {
			fmt.Fprintf(&b, "#EXTINF:%d, nodesc\n%05d.ts\n", SegmentSeconds, i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(b.Bytes())
	case reSegment.MatchString(p):
		m := reSegment.FindStringSubmatch(p)
		sess := m[1]
		seq, _ := strconv.Atoi(m[2])
		s.mu.Lock()
		first, ok := s.sessions[sess]
		s.fetched[fmt.Sprintf("%s/%d", sess, seq)]++
		s.mu.Unlock()
		if !ok {
			http.Error(w, "no session "+Token, http.StatusNotFound)
			return
		}
		if s.SegDelay > 0 {
			time.Sleep(s.SegDelay)
		}
		w.Header().Set("Content-Type", "video/MP2T")
		if seq < first {
			w.Write(make([]byte, 188)) // what real Plex sends before the offset
			return
		}
		size := 4096
		if s.Oversized {
			size = 40 << 20
		}
		body := bytes.Repeat([]byte{0x47}, size)
		copy(body[100:], fmt.Sprintf("seg %s %d token=%s", sess, seq, Token))
		w.Write(body)
	case p == "/video/:/transcode/universal/ping":
		w.WriteHeader(http.StatusOK)
	case p == "/video/:/transcode/universal/stop":
		sess := r.URL.Query().Get("session")
		s.mu.Lock()
		delete(s.sessions, sess)
		s.stopped = append(s.stopped, sess)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case p == "/transcode/sessions":
		s.mu.Lock()
		var list []map[string]any
		for k := range s.sessions {
			list = append(list, map[string]any{"key": k})
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"TranscodeSession": list}})
	default:
		s.problem("unexpected path: %s", p)
		http.Error(w, "unknown "+Token, http.StatusNotFound)
	}
}

// itemForSession finds the item a session was started for, from its
// start.m3u8 record. The fake only needs the length, so it keeps the
// longest item's length per session via the started list.
func (s *Server) itemForSession(sess string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess]; !ok {
		return Item{}, false
	}
	key := sessionItem(sess)
	it, ok := s.items[key]
	return it, ok
}

// Sessions started by the relay embed the item key as "...-k<key>-...".
var reSessKey = regexp.MustCompile(`-k(\d+)-`)

func sessionItem(sess string) string {
	if m := reSessKey.FindStringSubmatch(sess); m != nil {
		return m[1]
	}
	return "100"
}

func (s *Server) raw(it Item) map[string]any {
	m := map[string]any{
		"ratingKey": it.Key, "type": it.Type, "title": it.Title,
		"librarySectionID": it.Section, "duration": it.Seconds * 1000,
		"thumb":   "/library/metadata/" + it.Key + "/thumb/1",
		"summary": "secret summary " + Token, // must never reach a viewer
	}
	switch it.Type {
	case "episode":
		m["grandparentTitle"], m["parentIndex"], m["index"], m["parentRatingKey"] = it.Show, it.Season, it.Ep, it.Parent
	case "season":
		m["parentTitle"], m["index"], m["parentRatingKey"] = it.Show, it.Season, it.Parent
	}
	if it.Height > 0 {
		m["Media"] = []any{map[string]any{"height": it.Height}}
	}
	return m
}

func sortedKeys(m map[string]Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
