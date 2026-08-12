package hub

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/yaseensharaf/game-arena/internal/game"
)

// seat is one of the two player slots in a room.
type seat struct {
	client  *Client
	mark    game.Mark
	session SessionID
	present bool
	rematch bool // has this seat asked for a rematch
}

// Room owns exactly one match.
//
// Concurrency model: every field below is touched only by run(). Other
// goroutines interact by sending on the request channels, never by reaching
// into the struct. That makes the game state single-owner by construction —
// there is no lock to forget to take, because there is no shared access to
// protect. The cost is that every interaction is a channel round trip, which
// is irrelevant at the message rate a board game produces.
type Room struct {
	id    string
	log   *slog.Logger
	state *game.State
	seats [2]*seat

	// graceTimer runs while a seat is empty, giving a dropped player a window
	// to reconnect before the match is abandoned.
	graceTimer  *time.Timer
	gracePeriod time.Duration

	moves    chan moveRequest
	rejoins  chan rejoinRequest
	departs  chan *Client
	rematchs chan *Client
	shutdown chan struct{}

	done     chan struct{}
	onClose  func(*Room)
	onResult func(outcome game.Outcome)
	onReject func()
}

type moveRequest struct {
	client *Client
	index  int
}

type rejoinRequest struct {
	client *Client
	result chan bool
}

func newRoom(id string, a, b *Client, gracePeriod time.Duration, log *slog.Logger) *Room {
	r := &Room{
		id:          id,
		log:         log.With("room", id),
		state:       game.New(),
		gracePeriod: gracePeriod,
		moves:       make(chan moveRequest, 8),
		rejoins:     make(chan rejoinRequest, 2),
		departs:     make(chan *Client, 2),
		rematchs:    make(chan *Client, 2),
		shutdown:    make(chan struct{}),
		done:        make(chan struct{}),
	}
	r.seats[0] = &seat{client: a, mark: game.X, session: a.Session, present: true}
	r.seats[1] = &seat{client: b, mark: game.O, session: b.Session, present: true}
	return r
}

// ID returns the room identifier.
func (r *Room) ID() string { return r.id }

// Done is closed once the room's goroutine has exited.
func (r *Room) Done() <-chan struct{} { return r.done }

// run is the room's event loop and the only goroutine that touches its state.
func (r *Room) run() {
	defer func() {
		r.stopGrace()
		close(r.done)
		if r.onClose != nil {
			r.onClose(r)
		}
	}()

	r.announceStart()

	for {
		var graceFired <-chan time.Time
		if r.graceTimer != nil {
			graceFired = r.graceTimer.C
		}

		select {
		case req := <-r.moves:
			r.handleMove(req)

		case req := <-r.rejoins:
			req.result <- r.handleRejoin(req.client)

		case c := <-r.departs:
			if r.handleDepart(c) {
				return
			}

		case c := <-r.rematchs:
			r.handleRematch(c)

		case <-graceFired:
			r.log.Info("abandoning match: reconnect window expired")
			r.broadcast(message{Type: MsgOpponentLeft, Data: leftPayload{Reason: "reconnect timeout"}})
			r.closeAll()
			return

		case <-r.shutdown:
			r.broadcast(message{Type: MsgServerShutdown})
			r.closeAll()
			return
		}
	}
}

func (r *Room) announceStart() {
	for _, s := range r.seats {
		r.sendTo(s, message{Type: MsgAssigned, Data: assignedPayload{
			Mark:    s.mark.String(),
			Session: string(s.session),
			Room:    r.id,
		}})
	}
	r.broadcastState()
}

func (r *Room) handleMove(req moveRequest) {
	s := r.seatFor(req.client)
	if s == nil {
		return
	}

	if err := r.state.ApplyMove(s.mark, req.index); err != nil {
		// Rejections go back to the mover only. Telling the player why their
		// click did nothing is the difference between a bug report and an
		// understood rule.
		if r.onReject != nil {
			r.onReject()
		}
		r.sendTo(s, message{Type: MsgError, Data: errorPayload{Reason: err.Error()}})
		return
	}

	r.broadcastState()

	if r.state.Outcome.Terminal() {
		r.log.Info("match finished", "outcome", r.state.Outcome.String())
		if r.onResult != nil {
			r.onResult(r.state.Outcome)
		}
	}
}

// handleRejoin reattaches a reconnecting client to the seat matching its
// session, cancelling the abandonment timer. Returns false if the session does
// not belong to this room or the seat is already occupied.
func (r *Room) handleRejoin(c *Client) bool {
	s := r.seatForSession(c.Session)
	if s == nil || s.present {
		return false
	}

	s.client = c
	s.present = true
	r.stopGrace()
	r.log.Info("player reconnected", "mark", s.mark.String())

	r.sendTo(s, message{Type: MsgAssigned, Data: assignedPayload{
		Mark:    s.mark.String(),
		Session: string(s.session),
		Room:    r.id,
	}})
	r.sendTo(s, message{Type: MsgState, Data: r.state.Clone()})
	r.sendToOther(s, message{Type: MsgOpponentReconnected})
	return true
}

// handleDepart marks a seat empty. It returns true when the room should shut
// down: either both seats are gone, or the match had already finished so
// there's nothing to reconnect to.
func (r *Room) handleDepart(c *Client) bool {
	s := r.seatFor(c)
	if s == nil {
		return false
	}
	s.present = false
	s.rematch = false
	c.Close()

	if !r.anyPresent() {
		r.log.Info("closing room: both players gone")
		return true
	}

	if r.state.Outcome.Terminal() {
		r.log.Info("closing room: player left after match end")
		r.broadcast(message{Type: MsgOpponentLeft, Data: leftPayload{Reason: "opponent left"}})
		r.closeAll()
		return true
	}

	r.log.Info("player disconnected, holding seat", "mark", s.mark.String(),
		"grace", r.gracePeriod.String())
	r.sendToOther(s, message{Type: MsgOpponentDisconnected, Data: disconnectPayload{
		GraceSeconds: int(r.gracePeriod.Seconds()),
	}})
	r.startGrace()
	return false
}

// handleRematch resets the board once both players have asked for one. Asking
// is per-seat and reset on each new match, so a stale flag can't start a game
// nobody agreed to.
func (r *Room) handleRematch(c *Client) {
	s := r.seatFor(c)
	if s == nil || !r.state.Outcome.Terminal() {
		return
	}
	s.rematch = true
	r.sendToOther(s, message{Type: MsgRematchOffered, Data: rematchPayload{By: s.mark.String()}})

	if !(r.seats[0].rematch && r.seats[1].rematch) {
		return
	}
	if !r.seats[0].present || !r.seats[1].present {
		return
	}

	r.state.Reset()
	// Swap marks so the first-move advantage alternates between rematches.
	r.seats[0].mark, r.seats[1].mark = r.seats[1].mark, r.seats[0].mark
	r.seats[0].rematch, r.seats[1].rematch = false, false
	r.log.Info("rematch starting")
	r.announceStart()
}

func (r *Room) startGrace() {
	r.stopGrace()
	r.graceTimer = time.NewTimer(r.gracePeriod)
}

func (r *Room) stopGrace() {
	if r.graceTimer != nil {
		r.graceTimer.Stop()
		r.graceTimer = nil
	}
}

func (r *Room) anyPresent() bool {
	return r.seats[0].present || r.seats[1].present
}

func (r *Room) seatFor(c *Client) *seat {
	for _, s := range r.seats {
		if s.client == c && s.present {
			return s
		}
	}
	return nil
}

func (r *Room) seatForSession(id SessionID) *seat {
	for _, s := range r.seats {
		if s.session == id {
			return s
		}
	}
	return nil
}

func (r *Room) broadcastState() {
	r.broadcast(message{Type: MsgState, Data: r.state.Clone()})
}

func (r *Room) broadcast(m message) {
	payload, err := json.Marshal(m)
	if err != nil {
		r.log.Error("marshalling outbound message", "type", m.Type, "err", err)
		return
	}
	for _, s := range r.seats {
		if s.present {
			r.deliver(s, payload)
		}
	}
}

func (r *Room) sendTo(s *seat, m message) {
	if !s.present {
		return
	}
	payload, err := json.Marshal(m)
	if err != nil {
		r.log.Error("marshalling outbound message", "type", m.Type, "err", err)
		return
	}
	r.deliver(s, payload)
}

func (r *Room) sendToOther(from *seat, m message) {
	for _, s := range r.seats {
		if s != from && s.present {
			r.sendTo(s, m)
		}
	}
}

func (r *Room) deliver(s *seat, payload []byte) {
	if !s.client.trySend(payload) {
		r.log.Warn("dropping message: client buffer full", "mark", s.mark.String())
	}
}

func (r *Room) closeAll() {
	for _, s := range r.seats {
		if s.client != nil {
			s.client.Close()
		}
	}
}
