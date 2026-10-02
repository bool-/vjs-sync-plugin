package plex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// ReadTokenFile reads the token from a file that only its owner can read.
// The token never comes from a flag (visible in ps) or the environment
// (inherited by children, dumped on crashes).
func ReadTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("token file: not a regular file")
	}
	// Windows has no Unix permission bits; development only.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file: mode %v lets others read it; chmod 600 it", info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" || strings.ContainsAny(token, " \t\r\n") || len(token) > 256 {
		return "", errors.New("token file: empty or malformed")
	}
	return token, nil
}

// Level is one quality rung.
type Level struct {
	Height int
	Kbps   int
}

// Width is the 16:9 width for the level; Plex keeps the source's aspect.
func (l Level) Width() int { return l.Height * 16 / 9 / 2 * 2 }

// ParseLevels reads "480:1500,720:4000,1080:8000", strictly ascending.
func ParseLevels(s string) ([]Level, error) {
	var out []Level
	for _, part := range strings.Split(s, ",") {
		h, k, ok := strings.Cut(strings.TrimSpace(part), ":")
		height, err1 := strconv.Atoi(h)
		kbps, err2 := strconv.Atoi(k)
		if !ok || err1 != nil || err2 != nil || height < 144 || height > 2160 || kbps < 100 || kbps > 100000 {
			return nil, fmt.Errorf("levels: bad entry %q (want height:kbps)", part)
		}
		if n := len(out); n > 0 && (height <= out[n-1].Height || kbps <= out[n-1].Kbps) {
			return nil, errors.New("levels: must be strictly ascending in height and bitrate")
		}
		out = append(out, Level{Height: height, Kbps: kbps})
	}
	if len(out) == 0 || len(out) > 4 {
		return nil, errors.New("levels: give one to four")
	}
	return out, nil
}

// PreflightConfig is what the server expects of Plex.
type PreflightConfig struct {
	ServerID  string
	Libraries []int
	Levels    []Level
}

// PatchedVersion is the first PMS release without CVE-2025-69414.
const PatchedVersion = "1.42.2.10156"

// Preflight checks Plex before the server takes traffic. A non-nil error
// means do not start; warnings are logged and the server starts anyway.
func Preflight(ctx context.Context, c *Client, cfg PreflightConfig) (warnings []string, err error) {
	id, err := c.Identity(ctx)
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	// Checked before any call that needs the token's rights, so a token is
	// never used against a server that has changed hands.
	if cfg.ServerID == "" || id.MachineID != cfg.ServerID {
		return nil, errors.New("preflight: Plex machineIdentifier does not match -plex-server-id")
	}
	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, fmt.Errorf("preflight: token rejected or library list failed: %w", err)
	}
	have := map[int]bool{}
	for _, s := range sections {
		have[s.ID] = true
	}
	if len(cfg.Libraries) == 0 {
		return nil, errors.New("preflight: -plex-libraries is empty")
	}
	for _, l := range cfg.Libraries {
		if !have[l] {
			return nil, fmt.Errorf("preflight: library %d does not exist on the server", l)
		}
	}

	if olderThan(id.Version, PatchedVersion) {
		warnings = append(warnings, fmt.Sprintf("Plex %s is older than %s (CVE-2025-69414); update it", id.Version, PatchedVersion))
	}
	prefs, err := c.Prefs(ctx)
	if err != nil {
		warnings = append(warnings, "could not read Plex settings: "+err.Error())
		return warnings, nil
	}
	top, total := 0, 0
	for _, l := range cfg.Levels {
		total += l.Kbps
		if l.Kbps > top {
			top = l.Kbps
		}
	}
	if v, _ := strconv.Atoi(prefs["WanPerStreamMaxUploadRate"]); v != 0 && v < top {
		warnings = append(warnings, fmt.Sprintf("Plex limits remote streams to %d kbps, below the top level's %d", v, top))
	}
	if v, _ := strconv.Atoi(prefs["WanTotalMaxUploadRate"]); v != 0 && v < total {
		warnings = append(warnings, fmt.Sprintf("Plex limits total remote upload to %d kbps, below all levels together (%d)", v, total))
	}
	if v, _ := strconv.Atoi(prefs["WanPerUserStreamCount"]); v != 0 && v < len(cfg.Levels) {
		warnings = append(warnings, fmt.Sprintf("Plex allows %d remote streams per user; the relay may need %d", v, len(cfg.Levels)))
	}
	return warnings, nil
}

// olderThan compares dotted version prefixes numerically, ignoring any
// build suffix such as "-e5521bd8c".
func olderThan(v, min string) bool {
	v, _, _ = strings.Cut(v, "-")
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := range b {
		if i >= len(a) {
			return true
		}
		x, err1 := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if err1 != nil {
			return true
		}
		if x != y {
			return x < y
		}
	}
	return false
}
