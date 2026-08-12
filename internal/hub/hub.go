// Package hub matches players into rooms and routes their messages.
//
// Locking discipline: the hub's mutex guards only its bookkeeping maps, never
// game state, and is never held while sending on a room channel. Holding it
// across a channel send would let a room goroutine calling back into onRoomClosed
// deadlock against a caller waiting to hand that same room a message.
package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/yaseensharaf/game-arena/internal/game"
)

// Metrics receives lifecycle events. It is an interface so the hub does not
// depend on a metrics backend, and so tests can assert on what was recorded.
type Metrics interface {
	RoomOpened()
	RoomClosed(duration time.Duration)
	MatchFinished(outcome string)
	PlayerConnected()
	PlayerDisconnected()
	PlayerReconnected()
	MoveRejected()
}

// NopMetrics discards everything. Used in tests and when metrics are disabled.
type NopMetrics struct{}

func (NopMetrics) RoomOpened()              {}
func (NopMetrics) RoomClosed(time.Duration) {}
func (NopMetrics) MatchFinished(string)     {}
func (NopMetrics) PlayerConnected()         {}
func (NopMetrics) PlayerDisconnected()      {}
func (NopMetrics) PlayerReconnected()       {}
func (NopMetrics) MoveRejected()            {}

// Config tunes hub behaviour.
type Config struct {
	// GracePeriod is how long a seat is held open for a disconnected player
	// before the match is abandoned.
	GracePeriod time.Duration
	// SendBuffer is the per-client outbound queue depth.
	SendBuffer int
}

func (c Config) withDefaults() Config {
	if c.GracePeriod <= 0 {
		c.GracePeriod = 30 * time.Second
	}
	if c.SendBuffer <= 0 {
		c.SendBuffer = 16
	}
	return c
}

// Hub owns matchmaking and the set of live rooms.
type Hub struct {
	cfg     Config
	log     *slog.Logger
	metrics Metrics

	mu        sync.Mutex
	waiting   *Client
	rooms     map[string]*Room
	byClient  map[*Client]*Room
	bySession map[SessionID]*Room
	openedAt  map[*Room]time.Time
	closing   bool
}

// New returns a hub ready to accept players.
func New(cfg Config, log *slog.Logger, m Metrics) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if m == nil {
		m = NopMetrics{}
	}
	return &Hub{
		cfg:       cfg.withDefaults(),
		log:       log,
		metrics:   m,
		rooms:     make(map[string]*Room),
		byClient:  make(map[*Client]*Room),
		bySession: make(map[SessionID]*Room),
		openedAt:  make(map[*Room]time.Time),
	}
}

// NewClient builds a client bound to this hub's buffer size.
func (h *Hub) NewClient(session SessionID) *Client {
	if session == "" {
		session = NewSessionID()
	}
	return NewClient(session, h.cfg.SendBuffer)
}

// Join places a client into a match: it reattaches to an existing seat if the
// client's session is mid-match, otherwise it pairs with a waiting player or
// becomes the one waiting.
func (h *Hub) Join(c *Client) {
	h.metrics.PlayerConnected()

	if h.tryRejoin(c) {
		h.metrics.PlayerReconnected()
		return
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		send(c, message{Type: MsgServerShutdown})
		c.Close()
		return
	}

	if h.waiting == nil || h.waiting == c {
		h.waiting = c
		h.mu.Unlock()
		send(c, message{Type: MsgWaiting})
		return
	}

	opponent := h.waiting
	h.waiting = nil

	room := newRoom(newRoomID(), opponent, c, h.cfg.GracePeriod, h.log)
	room.onClose = h.onRoomClosed
	room.onResult = func(o game.Outcome) { h.metrics.MatchFinished(o.String()) }
	room.onReject = h.metrics.MoveRejected

	h.rooms[room.id] = room
	h.byClient[opponent] = room
	h.byClient[c] = room
	h.bySession[opponent.Session] = room
	h.bySession[c.Session] = room
	h.openedAt[room] = time.Now()
	h.mu.Unlock()

	h.metrics.RoomOpened()
	h.log.Info("match created", "room", room.id)
	go room.run()
}

// tryRejoin reattaches a returning session to its room. The room, not the hub,
// decides whether the seat is actually claimable — only the room goroutine can
// read seat state safely.
func (h *Hub) tryRejoin(c *Client) bool {
	h.mu.Lock()
	room, ok := h.bySession[c.Session]
	h.mu.Unlock()
	if !ok {
		return false
	}

	result := make(chan bool, 1)
	select {
	case room.rejoins <- rejoinRequest{client: c, result: result}:
	case <-room.done:
		return false
	}

	select {
	case ok := <-result:
		if !ok {
			return false
		}
	case <-room.done:
		return false
	}

	h.mu.Lock()
	h.byClient[c] = room
	h.mu.Unlock()
	return true
}

// Move forwards a move request to the client's room.
func (h *Hub) Move(c *Client, index int) {
	room := h.roomFor(c)
	if room == nil {
		return
	}
	select {
	case room.moves <- moveRequest{client: c, index: index}:
	case <-room.done:
	}
}

// Rematch forwards a rematch request to the client's room.
func (h *Hub) Rematch(c *Client) {
	room := h.roomFor(c)
	if room == nil {
		return
	}
	select {
	case room.rematchs <- c:
	case <-room.done:
	}
}

// Leave removes a client from matchmaking or notifies its room that the
// player's connection dropped.
func (h *Hub) Leave(c *Client) {
	h.metrics.PlayerDisconnected()

	h.mu.Lock()
	if h.waiting == c {
		h.waiting = nil
	}
	room, inRoom := h.byClient[c]
	delete(h.byClient, c)
	h.mu.Unlock()

	if !inRoom {
		c.Close()
		return
	}

	select {
	case room.departs <- c:
	case <-room.done:
		c.Close()
	}
}

// Shutdown closes every room and stops accepting new matches. It returns once
// all rooms have finished or ctx-equivalent timeout elapses.
func (h *Hub) Shutdown(timeout time.Duration) {
	h.mu.Lock()
	h.closing = true
	if h.waiting != nil {
		send(h.waiting, message{Type: MsgServerShutdown})
		h.waiting.Close()
		h.waiting = nil
	}
	rooms := make([]*Room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.Unlock()

	for _, r := range rooms {
		select {
		case r.shutdown <- struct{}{}:
		case <-r.done:
		default:
		}
	}

	deadline := time.After(timeout)
	for _, r := range rooms {
		select {
		case <-r.done:
		case <-deadline:
			h.log.Warn("shutdown timed out waiting for rooms")
			return
		}
	}
}

// Stats reports current occupancy, used by the metrics endpoint.
func (h *Hub) Stats() (rooms, waiting int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := 0
	if h.waiting != nil {
		w = 1
	}
	return len(h.rooms), w
}

func (h *Hub) roomFor(c *Client) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byClient[c]
}

// onRoomClosed is called from the room's goroutine as it exits. It must not
// send on any room channel, and the hub lock must not be held by the caller.
func (h *Hub) onRoomClosed(r *Room) {
	h.mu.Lock()
	delete(h.rooms, r.id)
	opened, hadOpen := h.openedAt[r]
	delete(h.openedAt, r)
	for _, s := range r.seats {
		delete(h.bySession, s.session)
		if s.client != nil {
			delete(h.byClient, s.client)
		}
	}
	h.mu.Unlock()

	if hadOpen {
		h.metrics.RoomClosed(time.Since(opened))
	}
	h.log.Info("room closed", "room", r.id)
}

func send(c *Client, m message) {
	payload, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.trySend(payload)
}

func newRoomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "room-fallback"
	}
	return hex.EncodeToString(b)
}
