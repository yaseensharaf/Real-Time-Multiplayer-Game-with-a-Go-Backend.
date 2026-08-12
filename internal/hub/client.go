package hub

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// SessionID identifies a player across connections, so a dropped socket can be
// reattached to the seat it left rather than starting a new match.
type SessionID string

// NewSessionID returns an unguessable session identifier. It's crypto/rand
// rather than math/rand because knowing another player's session would let you
// steal their seat on reconnect.
func NewSessionID() SessionID {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the OS entropy source is broken; there is
		// no safe fallback that preserves unguessability, so fail loudly.
		panic("hub: cannot read random bytes: " + err.Error())
	}
	return SessionID(hex.EncodeToString(b))
}

// Client is one connected player from the hub's point of view.
//
// The hub never touches a socket directly — it writes framed payloads to Send
// and lets the transport layer own the connection. That keeps the whole
// matchmaking and game layer testable without opening a port.
type Client struct {
	Session SessionID

	// send is buffered so that one slow reader cannot stall a room's
	// broadcast loop and, through it, the other player's game.
	send chan []byte

	closeOnce sync.Once
	closed    chan struct{}
}

// NewClient returns a client with the given session and outbound buffer size.
func NewClient(session SessionID, buffer int) *Client {
	return &Client{
		Session: session,
		send:    make(chan []byte, buffer),
		closed:  make(chan struct{}),
	}
}

// Out is the stream of payloads the transport layer must write to the socket.
// It is closed when the client is closed, which is what terminates the
// transport's write loop — without this, every disconnect would leak a
// goroutine blocked on a channel nobody will ever write to again.
func (c *Client) Out() <-chan []byte { return c.send }

// Done is closed when this client is closed.
func (c *Client) Done() <-chan struct{} { return c.closed }

// Close releases the client. Safe to call from multiple goroutines and more
// than once — both the read loop and the write loop race to close on error.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		close(c.send)
	})
}

// trySend delivers a payload without blocking. A full buffer means the peer is
// not draining fast enough; dropping is deliberate — the alternative is
// letting one stalled client freeze the room goroutine for everyone in it.
// Returns false if the payload was dropped.
func (c *Client) trySend(payload []byte) bool {
	select {
	case <-c.closed:
		return false
	default:
	}

	// A concurrent Close between the check above and the send below would
	// panic on a closed channel, so recover and report the drop instead.
	defer func() { _ = recover() }()

	select {
	case c.send <- payload:
		return true
	default:
		return false
	}
}
