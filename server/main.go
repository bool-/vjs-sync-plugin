// Command sync-server keeps video players in step. Clients join a room over
// a WebSocket; the server owns each room's timeline and broadcasts it.
// Party rooms take play/pause/seek from anyone in them; the scheduled room
// runs a showing on a fixed timetable and takes no commands.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxMessage = 1024
	// Clients ping every 5s; three missed pings and they are gone.
	idleTimeout = 15 * time.Second
	sendBuffer  = 32
	// Per-connection message budget: refill rate and burst.
	ratePerSecond = 10
	rateBurst     = 20
)

type inbound struct {
	Type     string   `json:"type"`
	Room     string   `json:"room"`
	T0       *float64 `json:"t0"`
	Action   string   `json:"action"`
	Position *float64 `json:"position"`
	Rev      uint64   `json:"rev"`
}

type pong struct {
	Type string  `json:"type"`
	T0   float64 `json:"t0"`
	T    float64 `json:"t"`
}

type errorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type client struct {
	conn      *wsConn
	send      chan []byte
	closeOnce sync.Once
	tokens    float64
	refilled  time.Time
}

// offer queues a message without blocking; a full queue means the client
// cannot keep up, so it is cut off.
func (c *client) offer(message []byte) {
	select {
	case c.send <- message:
	default:
		c.closeOnce.Do(func() { c.conn.Close() })
	}
}

func (c *client) reply(v any) {
	b, _ := json.Marshal(v)
	c.offer(b)
}

func (c *client) allow(now time.Time) bool {
	c.tokens += now.Sub(c.refilled).Seconds() * ratePerSecond
	if c.tokens > rateBurst {
		c.tokens = rateBurst
	}
	c.refilled = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *client) writeLoop() {
	for message := range c.send {
		if err := c.conn.WriteText(message); err != nil {
			c.closeOnce.Do(func() { c.conn.Close() })
			// Keep draining so offer never blocks; the reader will clean up.
		}
	}
}

func serveWS(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrade(w, r, maxMessage)
		if err != nil {
			return
		}
		c := &client{conn: conn, send: make(chan []byte, sendBuffer), tokens: rateBurst, refilled: time.Now()}
		go c.writeLoop()

		var room *Room
		defer func() {
			if room != nil {
				hub.leave(c, room)
			}
			// Nobody can offer to c any more: it is out of every room and
			// this goroutine was the only other sender.
			close(c.send)
			c.closeOnce.Do(func() { c.conn.Close() })
		}()

		for {
			raw, err := conn.ReadMessage(idleTimeout)
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, errProtocol) && !errors.Is(err, errTooLarge) {
					log.Printf("ws read: %v", err)
				}
				return
			}
			if !c.allow(time.Now()) {
				continue
			}
			var m inbound
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			switch m.Type {
			case "ping":
				if m.T0 != nil {
					c.reply(pong{Type: "pong", T0: *m.T0, T: nowMs()})
				}
			case "join":
				if room != nil {
					hub.leave(c, room)
					room = nil
				}
				if room = hub.join(c, m.Room); room == nil {
					c.reply(errorMessage{Type: "error", Message: "bad room name or server full"})
				}
			case "command":
				if room == nil {
					continue
				}
				if room.locked {
					c.reply(errorMessage{Type: "error", Message: "this room runs on a schedule"})
					continue
				}
				room.mu.Lock()
				if room.apply(m.Action, m.Position, nowMs()) {
					room.broadcast()
				}
				room.mu.Unlock()
			case "ready":
				if room == nil {
					continue
				}
				room.mu.Lock()
				if room.markReady(c, m.Rev) {
					room.start(nowMs())
					room.broadcast()
				}
				room.mu.Unlock()
			}
		}
	}
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	web := flag.String("web", ".", "repository root; serves example/ at / and client/ at /client/")
	src := flag.String("src", "https://vjs.zencdn.net/v/oceans.mp4", "media URL every room plays")
	srcType := flag.String("type", "", "media MIME type, if the URL does not make it obvious")
	scheduled := flag.String("scheduled-room", "drive-in", "name of the room on a timetable; empty for none")
	every := flag.Duration("show-every", 75*time.Second, "time between showings in the scheduled room")
	countdown := flag.Duration("countdown", 20*time.Second, "how long before a showing it is announced")
	maxRooms := flag.Int("max-rooms", 1000, "most rooms at once")
	flag.Parse()

	if *countdown >= *every {
		log.Fatal("-countdown must be shorter than -show-every")
	}

	// Windows can map .js to text/plain through the registry; module
	// scripts refuse to load unless it is JavaScript.
	mime.AddExtensionType(".js", "text/javascript; charset=utf-8")

	source := &Source{Src: *src, Type: *srcType}
	hub := newHub(source, *maxRooms)
	if *scheduled != "" {
		room := hub.addScheduled(*scheduled, source)
		// First showing starts one countdown after boot.
		go runSchedule(room, nowMs()+float64(countdown.Milliseconds()), *every, *countdown, nil)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", serveWS(hub))
	mux.Handle("/client/", http.StripPrefix("/client/", http.FileServer(http.Dir(filepath.Join(*web, "client")))))
	mux.Handle("/", http.FileServer(http.Dir(filepath.Join(*web, "example"))))

	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
