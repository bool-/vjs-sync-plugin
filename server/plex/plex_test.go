package plex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bool-/vjs-sync-plugin/server/plex"
	"github.com/bool-/vjs-sync-plugin/server/plex/plextest"
)

func client(t *testing.T) (*plex.Client, *plextest.Server) {
	t.Helper()
	fake := plextest.New()
	t.Cleanup(func() {
		fake.Close()
		for _, p := range fake.Problems() {
			t.Errorf("fake Plex saw: %s", p)
		}
	})
	c, err := plex.New(fake.URL, plextest.Token, fake.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

func noToken(t *testing.T, what string, s string) {
	t.Helper()
	if strings.Contains(s, plextest.Token) {
		t.Fatalf("token leaked in %s: %q", what, s)
	}
}

func TestNewRefusesNonHTTPS(t *testing.T) {
	for _, base := range []string{"http://plex.example:32400", "https://plex.example/path", "https://plex.example/?a=1", "plex.example"} {
		if _, err := plex.New(base, "tok", nil); err == nil {
			t.Errorf("accepted %q", base)
		}
	}
	if _, err := plex.New("https://plex.example:32400", " ", nil); err == nil {
		t.Error("accepted empty token")
	}
}

func TestPreflight(t *testing.T) {
	c, fake := client(t)
	levels, _ := plex.ParseLevels("480:1500,720:4000,1080:8000")
	cfg := plex.PreflightConfig{ServerID: plextest.ServerID, Libraries: []int{1, 2}, Levels: levels}
	ctx := context.Background()

	warn, err := plex.Preflight(ctx, c, cfg)
	if err != nil || len(warn) != 0 {
		t.Fatalf("clean server: warn=%v err=%v", warn, err)
	}

	bad := cfg
	bad.ServerID = "someone-else"
	if _, err := plex.Preflight(ctx, c, bad); err == nil || !strings.Contains(err.Error(), "machineIdentifier") {
		t.Fatalf("server pin not enforced: %v", err)
	}
	bad = cfg
	bad.Libraries = []int{1, 9}
	if _, err := plex.Preflight(ctx, c, bad); err == nil {
		t.Fatal("missing library accepted")
	}

	fake.Prefs["WanPerStreamMaxUploadRate"] = 4000
	fake.Prefs["WanTotalMaxUploadRate"] = 10000
	fake.Prefs["WanPerUserStreamCount"] = 2
	fake.Version = "1.41.0.1-abc"
	warn, err = plex.Preflight(ctx, c, cfg)
	if err != nil || len(warn) != 4 {
		t.Fatalf("want 4 warnings, got %d: %v (err %v)", len(warn), warn, err)
	}
}

func TestPreflightBadToken(t *testing.T) {
	fake := plextest.New()
	defer fake.Close()
	c, _ := plex.New(fake.URL, "wrong-token", fake.Client())
	fake.FailNext["/library/sections"] = 401
	levels, _ := plex.ParseLevels("720:4000")
	_, err := plex.Preflight(context.Background(), c, plex.PreflightConfig{ServerID: plextest.ServerID, Libraries: []int{1}, Levels: levels})
	if err == nil {
		t.Fatal("rejected token passed preflight")
	}
	noToken(t, "preflight error", err.Error())
}

func TestMetadataAndChildren(t *testing.T) {
	c, _ := client(t)
	ctx := context.Background()
	it, err := c.Metadata(ctx, "211")
	if err != nil {
		t.Fatal(err)
	}
	if it.Type != "episode" || it.Show != "The Show" || it.Season != 1 || it.Episode != 1 || it.Duration != 120 || it.SectionID != 2 || it.Height != 1080 {
		t.Fatalf("episode parsed as %+v", it)
	}
	kids, err := c.Children(ctx, "210")
	if err != nil || len(kids) != 4 || kids[3].Episode != 4 {
		t.Fatalf("children: %v %+v", err, kids)
	}
	if _, err := c.Metadata(ctx, "../../etc"); err == nil {
		t.Fatal("non-numeric key accepted")
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	c, fake := client(t)
	ctx := context.Background()
	fake.FailNext["/library/metadata/100"] = 500
	_, err := c.Metadata(ctx, "100")
	if err == nil {
		t.Fatal("expected error")
	}
	noToken(t, "500 error", err.Error())

	fake.FailNext["/library/metadata/100"] = 302
	_, err = c.Metadata(ctx, "100")
	if err == nil || !strings.Contains(err.Error(), "302") && !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect followed or misreported: %v", err)
	}
	noToken(t, "redirect error", err.Error())

	// A connection failure: the *url.Error would include the URL.
	dead, _ := plex.New("https://127.0.0.1:1", plextest.Token, nil)
	_, err = dead.Metadata(ctx, "100")
	if err == nil {
		t.Fatal("expected connection error")
	}
	noToken(t, "dial error", err.Error())
	if strings.Contains(err.Error(), "?") {
		t.Fatalf("error carries a query string: %v", err)
	}
}

func TestTranscodeFlow(t *testing.T) {
	c, fake := client(t)
	ctx := context.Background()
	tr, err := c.StartTranscode(ctx, plex.TranscodeParams{Key: "100", Session: "ms-test-k100-720-a1", Width: 1280, Height: 720, Kbps: 4000, Offset: 300})
	if err != nil {
		t.Fatal(err)
	}
	segs, err := c.Playlist(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 120 || segs[60].Start != 300 {
		t.Fatalf("want 120 segments with seg 60 at 300 s, got %d / %v", len(segs), segs[60].Start)
	}
	if _, err := c.FetchSegment(ctx, tr, segs[0], 1<<20); !errors.Is(err, plex.ErrEmptySegment) {
		t.Fatalf("segment before the offset should be empty, got %v", err)
	}
	b, err := c.FetchSegment(ctx, tr, segs[61], 1<<20)
	if err != nil || len(b) < plex.MinSegment {
		t.Fatalf("segment after the offset: %v (%d bytes)", err, len(b))
	}
	fake.Oversized = true
	if _, err := c.FetchSegment(ctx, tr, segs[62], 1<<20); !errors.Is(err, plex.ErrTooLarge) {
		t.Fatalf("oversized segment: %v", err)
	}
	if err := c.Stop(ctx, tr.Session); err != nil {
		t.Fatal(err)
	}
	if got := fake.Stopped(); len(got) != 1 || got[0] != tr.Session {
		t.Fatalf("stopped %v", got)
	}
}

func TestSessionIDsAreValidated(t *testing.T) {
	c, _ := client(t)
	for _, s := range []string{"", "short", "UPPER-case-session", "has/slash-session", "has..dots-session"} {
		if _, err := c.StartTranscode(context.Background(), plex.TranscodeParams{Key: "100", Session: s, Width: 1280, Height: 720, Kbps: 4000}); err == nil {
			t.Errorf("session %q accepted", s)
		}
	}
}

func TestReadTokenFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	os.WriteFile(p, []byte("abc123\n"), 0o600)
	if tok, err := plex.ReadTokenFile(p); err != nil || tok != "abc123" {
		t.Fatalf("got %q %v", tok, err)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(p, 0o644)
		if _, err := plex.ReadTokenFile(p); err == nil {
			t.Fatal("world-readable token file accepted")
		}
		os.Chmod(p, 0o600)
	}
	os.WriteFile(p, []byte("  \n"), 0o600)
	if _, err := plex.ReadTokenFile(p); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestParseLevels(t *testing.T) {
	if l, err := plex.ParseLevels("480:1500,720:4000,1080:8000"); err != nil || len(l) != 3 || l[1].Width() != 1280 {
		t.Fatalf("%v %v", l, err)
	}
	for _, bad := range []string{"", "720", "720:4000,480:1500", "720:4000,720:5000", "99:100", "720:x"} {
		if _, err := plex.ParseLevels(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
