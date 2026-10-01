package main

import (
	"encoding/json"
	"math"
	"regexp"
	"sync"
	"time"
)

// Source is what a room plays. Type is optional; clients guess from the URL.
type Source struct {
	Src  string `json:"src"`
	Type string `json:"type,omitempty"`
}

// Room owns one shared timeline: the media is at position seconds at server
// time at (epoch ms) and advances in real time from then unless paused.
// Until at it holds at position: that is how a room waits for its viewers to
// buffer, and how a scheduled showing counts down.
type Room struct {
	name   string
	locked bool
	src    *Source

	mu       sync.Mutex
	paused   bool
	position float64
	at       float64
	rev      uint64
	clients  map[*client]struct{}

	// After a play or seek the room waits until every viewer reports it is
	// buffered at the new spot, or until maxWait runs out.
	waiting   bool
	ready     map[*client]struct{}
	waitTimer *time.Timer
	maxWait   time.Duration
}

type stateMessage struct {
	Type     string  `json:"type"`
	Room     string  `json:"room"`
	Rev      uint64  `json:"rev"`
	Paused   bool    `json:"paused"`
	Position float64 `json:"position"`
	At       float64 `json:"at"`
	Locked   bool    `json:"locked"`
	Waiting  bool    `json:"waiting"`
	Src      *Source `json:"src,omitempty"`
}

const (
	// maxPosition caps client-supplied positions at about 115 days.
	maxPosition = 1e7
	// startMargin puts the start far enough ahead that the broadcast
	// reaches everyone before it happens.
	startMargin = 300 * time.Millisecond
	// defaultMaxWait is how long a room waits for slow viewers before
	// starting without them; they catch up on their own.
	defaultMaxWait = 5 * time.Second
)

func nowMs() float64 {
	return float64(time.Now().UnixNano()) / 1e6
}

func newRoom(name string, src *Source, locked bool) *Room {
	return &Room{name: name, src: src, locked: locked, paused: true, at: nowMs(),
		clients: map[*client]struct{}{}, maxWait: defaultMaxWait}
}

func (r *Room) positionAt(t float64) float64 {
	if r.paused || t < r.at {
		return r.position
	}
	return r.position + (t-r.at)/1000
}

// apply runs a play, pause or seek at server time t and reports whether the
// state changed. A nil position means "wherever the room is now".
// Callers hold r.mu.
func (r *Room) apply(action string, position *float64, t float64) bool {
	current := r.positionAt(t)
	target := current
	if position != nil {
		target = math.Min(math.Max(*position, 0), maxPosition)
	}
	paused := r.paused
	switch action {
	case "play":
		paused = false
	case "pause":
		paused = true
	case "seek":
	default:
		return false
	}
	// Everyone hitting pause at the end of the film is one change, not N.
	if paused == r.paused && math.Abs(target-current) < 0.01 {
		return false
	}
	r.paused, r.position, r.at = paused, target, t
	r.rev++
	r.stopWaiting()
	if !paused {
		r.wait(t)
	}
	return true
}

// wait holds the room at its position until every viewer is ready or
// maxWait passes. Callers hold r.mu.
func (r *Room) wait(t float64) {
	r.waiting = true
	r.ready = map[*client]struct{}{}
	r.at = t + float64(r.maxWait.Milliseconds())
	rev := r.rev
	r.waitTimer = time.AfterFunc(r.maxWait, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.waiting && r.rev == rev {
			r.start(nowMs())
			r.broadcast()
		}
	})
}

func (r *Room) stopWaiting() {
	if r.waitTimer != nil {
		r.waitTimer.Stop()
		r.waitTimer = nil
	}
	r.waiting = false
	r.ready = nil
}

// start ends the wait: everyone plays from the held position at t plus a
// margin. Callers hold r.mu.
func (r *Room) start(t float64) {
	r.stopWaiting()
	r.at = t + float64(startMargin.Milliseconds())
	r.rev++
}

// markReady records that c is buffered for state rev and reports whether
// that was the last viewer the room was waiting on. Callers hold r.mu.
func (r *Room) markReady(c *client, rev uint64) bool {
	if !r.waiting || rev != r.rev {
		return false
	}
	if _, ok := r.clients[c]; ok {
		r.ready[c] = struct{}{}
	}
	return r.allReady()
}

func (r *Room) allReady() bool {
	if !r.waiting {
		return false
	}
	for c := range r.clients {
		if _, ok := r.ready[c]; !ok {
			return false
		}
	}
	return true
}

// schedule points a locked room at a showtime. Callers hold r.mu.
func (r *Room) schedule(showtime float64) bool {
	if !r.paused && r.position == 0 && r.at == showtime {
		return false
	}
	r.paused, r.position, r.at = false, 0, showtime
	r.rev++
	return true
}

// snapshot encodes the state message. Callers hold r.mu.
func (r *Room) snapshot() []byte {
	b, _ := json.Marshal(stateMessage{
		Type: "state", Room: r.name, Rev: r.rev, Paused: r.paused,
		Position: r.position, At: r.at, Locked: r.locked, Waiting: r.waiting, Src: r.src,
	})
	return b
}

// broadcast sends the current state to everyone in the room. A client too
// slow to keep up is disconnected rather than allowed to stall the room.
// Callers hold r.mu.
func (r *Room) broadcast() {
	message := r.snapshot()
	for c := range r.clients {
		c.offer(message)
	}
}

// showFor returns the showtime a scheduled room should announce at time t:
// the current showing, until it has run for every-countdown, then the next
// one so viewers get a countdown. Showings start at anchor and repeat
// every `every`.
func showFor(t, anchor float64, every, countdown time.Duration) float64 {
	period := float64(every.Milliseconds())
	k := math.Floor((t - anchor) / period)
	current := anchor + k*period
	if t-current < period-float64(countdown.Milliseconds()) {
		return current
	}
	return current + period
}

var roomName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Hub holds the rooms. Party rooms appear on first join and vanish when
// the last viewer leaves; scheduled rooms live as long as the server.
type Hub struct {
	mu       sync.Mutex
	rooms    map[string]*Room
	src      *Source
	maxRooms int
	maxWait  time.Duration
}

func newHub(src *Source, maxRooms int) *Hub {
	return &Hub{rooms: map[string]*Room{}, src: src, maxRooms: maxRooms, maxWait: defaultMaxWait}
}

func (h *Hub) addScheduled(name string, src *Source) *Room {
	room := newRoom(name, src, true)
	h.mu.Lock()
	h.rooms[name] = room
	h.mu.Unlock()
	return room
}

// join puts c in the named room and sends it the room's state. It returns
// nil when the name is bad or the server is full.
func (h *Hub) join(c *client, name string) *Room {
	if !roomName.MatchString(name) {
		return nil
	}
	h.mu.Lock()
	room, ok := h.rooms[name]
	if !ok {
		if len(h.rooms) >= h.maxRooms {
			h.mu.Unlock()
			return nil
		}
		room = newRoom(name, h.src, false)
		room.maxWait = h.maxWait
		h.rooms[name] = room
	}
	room.mu.Lock()
	h.mu.Unlock()
	room.clients[c] = struct{}{}
	c.offer(room.snapshot())
	room.mu.Unlock()
	return room
}

func (h *Hub) leave(c *client, room *Room) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room.mu.Lock()
	defer room.mu.Unlock()
	delete(room.clients, c)
	if len(room.clients) == 0 {
		room.stopWaiting()
		if !room.locked && h.rooms[room.name] == room {
			delete(h.rooms, room.name)
		}
		return
	}
	// The viewer everyone was waiting on may just have left.
	if room.allReady() {
		room.start(nowMs())
		room.broadcast()
	}
}

// runSchedule keeps a locked room pointed at the right showtime.
func runSchedule(room *Room, anchor float64, every, countdown time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		show := showFor(nowMs(), anchor, every, countdown)
		room.mu.Lock()
		if room.schedule(show) {
			room.broadcast()
		}
		room.mu.Unlock()
		select {
		case <-tick.C:
		case <-stop:
			return
		}
	}
}
