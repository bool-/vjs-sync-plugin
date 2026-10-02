// Package plex is the only code that talks to Plex. It makes a fixed set of
// calls, sends the token only as a request header, refuses redirects, and
// returns errors that name the call and the path but never a query string,
// so the token cannot surface in an error, a log line or a response.
package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	clientID  = "media-sync"
	userAgent = "media-sync/1"
	// Plex's transcoder lives under this prefix; every playlist and segment
	// URL the relay follows must stay inside it.
	transcodePrefix = "/video/:/transcode/universal/session/"
)

// Error is what every call returns on failure. It never carries a query
// string or a response body, so it is safe to log.
type Error struct {
	Op     string
	Path   string
	Status int
	Err    error
}

func (e *Error) Error() string {
	switch {
	case e.Status != 0:
		return fmt.Sprintf("plex %s %s: HTTP %d", e.Op, e.Path, e.Status)
	case e.Err != nil:
		return fmt.Sprintf("plex %s %s: %s", e.Op, e.Path, safeCause(e.Err))
	default:
		return fmt.Sprintf("plex %s %s: failed", e.Op, e.Path)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// safeCause reduces a transport error to its kind. A *url.Error's message
// includes the full URL, so it is never used directly.
func safeCause(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, errRedirect):
		return "redirect refused"
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "connection failed"
}

var errRedirect = errors.New("redirect refused")

// ErrTooLarge means a response was bigger than the caller allows.
var ErrTooLarge = errors.New("response too large")

// Client calls one Plex server.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New returns a client for an https base URL such as
// https://1-2-3-4.abc.plex.direct:32400. httpClient may be nil; tests pass
// one that trusts their own certificate.
func New(base, token string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, errors.New("plex: base must be an https URL with no path or query")
	}
	u.Path = ""
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("plex: empty token")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	hc := *httpClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	if hc.Transport == nil {
		hc.Transport = &http.Transport{
			Proxy:                 nil,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	return &Client{base: u, token: strings.TrimSpace(token), http: &hc}, nil
}

// Host is the server's host:port, for logs.
func (c *Client) Host() string { return c.base.Host }

func (c *Client) url(path string, q url.Values) *url.URL {
	u := *c.base
	u.Path = path
	u.RawQuery = q.Encode()
	return &u
}

// own reports whether u is on this server and under the transcoder's
// session prefix: the only foreign URLs the relay will follow.
func (c *Client) own(u *url.URL, session string) bool {
	return u.Scheme == c.base.Scheme && u.Host == c.base.Host && u.User == nil &&
		strings.HasPrefix(u.EscapedPath(), transcodePrefix+url.PathEscape(session)+"/") &&
		!strings.Contains(u.Path, "..")
}

func (c *Client) do(ctx context.Context, op string, u *url.URL, accept string, extra http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Error{Op: op, Path: u.Path, Err: err}
	}
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Plex-Product", "media-sync")
	req.Header.Set("X-Plex-Version", "1")
	req.Header.Set("X-Plex-Platform", "Chrome")
	req.Header.Set("X-Plex-Client-Identifier", clientID)
	for k, v := range extra {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Path: u.Path, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &Error{Op: op, Path: u.Path, Status: resp.StatusCode}
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, op, path string, q url.Values, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.url(path, q)
	resp, err := c.do(ctx, op, u, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v); err != nil {
		return &Error{Op: op, Path: u.Path, Err: errors.New("bad json")}
	}
	return nil
}

func (c *Client) bytes(ctx context.Context, op string, u *url.URL, accept string, max int64, extra http.Header) ([]byte, string, error) {
	resp, err := c.do(ctx, op, u, accept, extra)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", &Error{Op: op, Path: u.Path, Err: err}
	}
	if int64(len(b)) > max {
		return nil, "", &Error{Op: op, Path: u.Path, Err: ErrTooLarge}
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// Identity is the server's /identity answer.
type Identity struct {
	MachineID string
	Version   string
}

// Identity needs no special rights; it confirms the server is the one we
// expect before the token is used for anything else.
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var r struct {
		MediaContainer struct {
			MachineIdentifier string `json:"machineIdentifier"`
			Version           string `json:"version"`
		}
	}
	if err := c.json(ctx, "identity", "/identity", nil, &r); err != nil {
		return Identity{}, err
	}
	return Identity{MachineID: r.MediaContainer.MachineIdentifier, Version: r.MediaContainer.Version}, nil
}

// Prefs returns the server's settings as strings, by id.
func (c *Client) Prefs(ctx context.Context) (map[string]string, error) {
	var r struct {
		MediaContainer struct {
			Setting []struct {
				ID    string `json:"id"`
				Value any    `json:"value"`
			}
		}
	}
	if err := c.json(ctx, "prefs", "/:/prefs", nil, &r); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range r.MediaContainer.Setting {
		out[s.ID] = fmt.Sprint(s.Value)
	}
	return out, nil
}

// Section is a library.
type Section struct {
	ID    int
	Type  string
	Title string
}

// Sections lists the libraries; it is also the authenticated call the
// preflight uses to prove the token works.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var r struct {
		MediaContainer struct {
			Directory []struct {
				Key   string `json:"key"`
				Type  string `json:"type"`
				Title string `json:"title"`
			}
		}
	}
	if err := c.json(ctx, "sections", "/library/sections", nil, &r); err != nil {
		return nil, err
	}
	var out []Section
	for _, d := range r.MediaContainer.Directory {
		id, err := strconv.Atoi(d.Key)
		if err != nil {
			continue
		}
		out = append(out, Section{ID: id, Type: d.Type, Title: d.Title})
	}
	return out, nil
}

// Item is a movie, show, season or episode.
type Item struct {
	Key       string  `json:"key"`
	Type      string  `json:"type"`
	Title     string  `json:"title"`
	Year      int     `json:"year,omitempty"`
	Show      string  `json:"show,omitempty"`
	Season    int     `json:"season,omitempty"`
	Episode   int     `json:"episode,omitempty"`
	Duration  float64 `json:"duration,omitempty"`
	Height    int     `json:"height,omitempty"`
	SectionID int     `json:"-"`
	ParentKey string  `json:"parentKey,omitempty"`
	thumb     string
}

// HasThumb reports whether the item has poster art.
func (i Item) HasThumb() bool { return i.thumb != "" }

type rawItem struct {
	RatingKey        string `json:"ratingKey"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	GrandparentTitle string `json:"grandparentTitle"`
	ParentTitle      string `json:"parentTitle"`
	ParentIndex      int    `json:"parentIndex"`
	Index            int    `json:"index"`
	Duration         int64  `json:"duration"`
	LibrarySectionID any    `json:"librarySectionID"`
	ParentRatingKey  string `json:"parentRatingKey"`
	Thumb            string `json:"thumb"`
	Media            []struct {
		Height          int    `json:"height"`
		VideoResolution string `json:"videoResolution"`
	} `json:"Media"`
}

func (r rawItem) item() Item {
	it := Item{
		Key: r.RatingKey, Type: r.Type, Title: r.Title, Year: r.Year,
		Duration: float64(r.Duration) / 1000, ParentKey: r.ParentRatingKey, thumb: r.Thumb,
	}
	switch r.Type {
	case "episode":
		it.Show, it.Season, it.Episode = r.GrandparentTitle, r.ParentIndex, r.Index
	case "season":
		it.Show, it.Season = r.ParentTitle, r.Index
	}
	switch v := r.LibrarySectionID.(type) {
	case float64:
		it.SectionID = int(v)
	case string:
		it.SectionID, _ = strconv.Atoi(v)
	}
	if len(r.Media) > 0 {
		it.Height = resolutionTier(r.Media[0].VideoResolution, r.Media[0].Height)
	}
	return it
}

// resolutionTier is the quality tier Plex files an item under ("1080",
// "720", "4k", "sd"), not its pixel height: a scope 1080p film is about 800
// pixels tall but should still offer the 1080p level.
func resolutionTier(res string, height int) int {
	switch strings.ToLower(res) {
	case "4k":
		return 2160
	case "sd":
		return 480
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(res), "p")); err == nil && n > 0 {
		return n
	}
	return height
}

// ValidKey reports whether s looks like a Plex ratingKey. Keys go into
// request paths, so anything else is refused before a call is made.
func ValidKey(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var errBadKey = errors.New("plex: invalid key")

// Metadata fetches one item.
func (c *Client) Metadata(ctx context.Context, key string) (Item, error) {
	if !ValidKey(key) {
		return Item{}, errBadKey
	}
	var r struct {
		MediaContainer struct {
			LibrarySectionID any       `json:"librarySectionID"`
			Metadata         []rawItem `json:"Metadata"`
		}
	}
	if err := c.json(ctx, "metadata", "/library/metadata/"+key, nil, &r); err != nil {
		return Item{}, err
	}
	if len(r.MediaContainer.Metadata) == 0 {
		return Item{}, &Error{Op: "metadata", Path: "/library/metadata/" + key, Status: http.StatusNotFound}
	}
	raw := r.MediaContainer.Metadata[0]
	if raw.LibrarySectionID == nil {
		raw.LibrarySectionID = r.MediaContainer.LibrarySectionID
	}
	return raw.item(), nil
}

// Children lists a show's seasons or a season's episodes.
func (c *Client) Children(ctx context.Context, key string) ([]Item, error) {
	if !ValidKey(key) {
		return nil, errBadKey
	}
	var r struct {
		MediaContainer struct {
			LibrarySectionID any       `json:"librarySectionID"`
			Metadata         []rawItem `json:"Metadata"`
		}
	}
	if err := c.json(ctx, "children", "/library/metadata/"+key+"/children", nil, &r); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(r.MediaContainer.Metadata))
	for _, raw := range r.MediaContainer.Metadata {
		if raw.LibrarySectionID == nil {
			raw.LibrarySectionID = r.MediaContainer.LibrarySectionID
		}
		out = append(out, raw.item())
	}
	return out, nil
}

// Search finds movies, shows and episodes by title.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Item, error) {
	var r struct {
		MediaContainer struct {
			Hub []struct {
				Type     string    `json:"type"`
				Metadata []rawItem `json:"Metadata"`
			}
		}
	}
	q := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}}
	if err := c.json(ctx, "search", "/hubs/search", q, &r); err != nil {
		return nil, err
	}
	var out []Item
	for _, h := range r.MediaContainer.Hub {
		if h.Type != "movie" && h.Type != "show" && h.Type != "episode" {
			continue
		}
		for _, raw := range h.Metadata {
			out = append(out, raw.item())
		}
	}
	return out, nil
}

// Poster returns an item's art, resized by Plex, as JPEG.
func (c *Client) Poster(ctx context.Context, it Item, width, height int) ([]byte, error) {
	if it.thumb == "" || !strings.HasPrefix(it.thumb, "/library/") {
		return nil, &Error{Op: "poster", Path: "/photo/:/transcode", Status: http.StatusNotFound}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	q := url.Values{"url": {it.thumb}, "width": {strconv.Itoa(width)}, "height": {strconv.Itoa(height)}, "minSize": {"1"}, "format": {"jpeg"}}
	b, ct, err := c.bytes(ctx, "poster", c.url("/photo/:/transcode", q), "image/jpeg", 2<<20, nil)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ct, "image/") {
		return nil, &Error{Op: "poster", Path: "/photo/:/transcode", Err: errors.New("not an image")}
	}
	return b, nil
}

// TranscodeParams starts one quality level of one item.
type TranscodeParams struct {
	Key     string
	Session string
	Width   int
	Height  int
	Kbps    int
	Offset  int // seconds into the item
}

// Transcode is a running Plex transcode session.
type Transcode struct {
	Session  string
	Playlist *url.URL // the media playlist, on this server, inside the session
}

func sessionHeaders(session string) http.Header {
	return http.Header{
		"X-Plex-Session-Identifier": {session},
		"X-Plex-Client-Identifier":  {clientID + "-" + session},
	}
}

// ValidSession reports whether s is safe to use as a Plex session ID.
func ValidSession(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// StartTranscode asks Plex to transcode an item at one quality level from
// an offset, and returns where its media playlist lives.
func (c *Client) StartTranscode(ctx context.Context, p TranscodeParams) (*Transcode, error) {
	if !ValidKey(p.Key) || !ValidSession(p.Session) {
		return nil, errBadKey
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	q := url.Values{
		"path": {"/library/metadata/" + p.Key}, "mediaIndex": {"0"}, "partIndex": {"0"},
		"protocol": {"hls"}, "fastSeek": {"1"}, "directPlay": {"0"}, "directStream": {"1"},
		"directStreamAudio": {"0"}, "videoQuality": {"100"},
		"videoResolution": {fmt.Sprintf("%dx%d", p.Width, p.Height)},
		"maxVideoBitrate": {strconv.Itoa(p.Kbps)}, "location": {"wan"},
		"session": {p.Session}, "offset": {strconv.Itoa(p.Offset)}, "copyts": {"1"},
		"subtitles": {"none"},
	}
	h := sessionHeaders(p.Session)
	// The decision call primes the session the way Plex's own clients do.
	if resp, err := c.do(ctx, "decision", c.url("/video/:/transcode/universal/decision", q), "application/json", h); err != nil {
		return nil, err
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
	start := c.url("/video/:/transcode/universal/start.m3u8", q)
	body, _, err := c.bytes(ctx, "start", start, "*/*", 64<<10, h)
	if err != nil {
		return nil, err
	}
	var ref string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			ref = line
			break
		}
	}
	if ref == "" {
		return nil, &Error{Op: "start", Path: start.Path, Err: errors.New("no variant in master playlist")}
	}
	u, err := start.Parse(ref)
	if err != nil || !c.own(u, p.Session) {
		return nil, &Error{Op: "start", Path: start.Path, Err: errors.New("variant outside the session")}
	}
	u.RawQuery = ""
	return &Transcode{Session: p.Session, Playlist: u}, nil
}

// Segment is one entry in a media playlist.
type Segment struct {
	URL      *url.URL
	Start    float64
	Duration float64
}

// Playlist fetches and parses a session's media playlist. Every segment URL
// must resolve inside the session, or the whole playlist is refused.
func (c *Client) Playlist(ctx context.Context, t *Transcode) ([]Segment, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body, _, err := c.bytes(ctx, "playlist", t.Playlist, "*/*", 4<<20, sessionHeaders(t.Session))
	if err != nil {
		return nil, err
	}
	var segs []Segment
	var start, dur float64
	pending := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			d, err := strconv.ParseFloat(v, 64)
			if err != nil || d <= 0 || d > 60 {
				return nil, &Error{Op: "playlist", Path: t.Playlist.Path, Err: errors.New("bad EXTINF")}
			}
			dur, pending = d, true
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if !pending {
				return nil, &Error{Op: "playlist", Path: t.Playlist.Path, Err: errors.New("segment without EXTINF")}
			}
			u, err := t.Playlist.Parse(line)
			if err != nil || !c.own(u, t.Session) {
				return nil, &Error{Op: "playlist", Path: t.Playlist.Path, Err: errors.New("segment outside the session")}
			}
			u.RawQuery = ""
			segs = append(segs, Segment{URL: u, Start: start, Duration: dur})
			start += dur
			pending = false
		}
	}
	if len(segs) == 0 {
		return nil, &Error{Op: "playlist", Path: t.Playlist.Path, Err: errors.New("empty playlist")}
	}
	return segs, nil
}

// MinSegment is the smallest real segment. Below it Plex has handed back
// an empty one (it does for anything before a session's start offset).
const MinSegment = 2048

// ErrEmptySegment means Plex answered with a placeholder, not media.
var ErrEmptySegment = errors.New("empty segment")

// FetchSegment downloads one segment of a session, at most max bytes.
func (c *Client) FetchSegment(ctx context.Context, t *Transcode, s Segment, max int64) ([]byte, error) {
	if !c.own(s.URL, t.Session) {
		return nil, &Error{Op: "segment", Path: s.URL.Path, Err: errors.New("segment outside the session")}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, ct, err := c.bytes(ctx, "segment", s.URL, "*/*", max, sessionHeaders(t.Session))
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ct, "video/") {
		return nil, &Error{Op: "segment", Path: s.URL.Path, Err: errors.New("not video")}
	}
	if len(b) < MinSegment {
		return nil, &Error{Op: "segment", Path: s.URL.Path, Err: ErrEmptySegment}
	}
	return b, nil
}

// Ping keeps a session alive.
func (c *Client) Ping(ctx context.Context, session string) error {
	return c.sessionCall(ctx, "ping", "/video/:/transcode/universal/ping", session)
}

// Stop ends a session.
func (c *Client) Stop(ctx context.Context, session string) error {
	return c.sessionCall(ctx, "stop", "/video/:/transcode/universal/stop", session)
}

func (c *Client) sessionCall(ctx context.Context, op, path, session string) error {
	if !ValidSession(session) {
		return errBadKey
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.do(ctx, op, c.url(path, url.Values{"session": {session}}), "*/*", sessionHeaders(session))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return nil
}

// Sessions lists the keys of the transcode sessions running on the server.
func (c *Client) Sessions(ctx context.Context) ([]string, error) {
	var r struct {
		MediaContainer struct {
			TranscodeSession []struct {
				Key string `json:"key"`
			}
		}
	}
	if err := c.json(ctx, "sessions", "/transcode/sessions", nil, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.MediaContainer.TranscodeSession {
		out = append(out, s.Key)
	}
	return out, nil
}
