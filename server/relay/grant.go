package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Grants are HMAC-signed path segments tying media requests to one
// connection in one room until an expiry. The key is random per process and
// never leaves memory, so a restart invalidates every grant; clients get
// new ones when they reconnect.
type Grants struct {
	key []byte
	now func() time.Time
}

// NewGrants makes a signer with a fresh random key.
func NewGrants(now func() time.Time) (*Grants, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &Grants{key: key, now: now}, nil
}

var b64 = base64.RawURLEncoding

func (g *Grants) mac(payload string) []byte {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Issue signs room|conn|expiry.
func (g *Grants) Issue(room string, conn uint64, ttl time.Duration) string {
	payload := room + "|" + strconv.FormatUint(conn, 10) + "|" + strconv.FormatInt(g.now().Add(ttl).Unix(), 10)
	return b64.EncodeToString([]byte(payload)) + "." + b64.EncodeToString(g.mac(payload))
}

// Check verifies a grant in constant time and returns its room and
// connection. Expired, malformed and forged grants all fail the same way.
func (g *Grants) Check(s string) (room string, conn uint64, ok bool) {
	if len(s) > 256 {
		return "", 0, false
	}
	p, sig, found := strings.Cut(s, ".")
	if !found {
		return "", 0, false
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return "", 0, false
	}
	got, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(got, g.mac(string(payload))) {
		return "", 0, false
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", 0, false
	}
	conn, err = strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || g.now().Unix() >= exp {
		return "", 0, false
	}
	return parts[0], conn, true
}
