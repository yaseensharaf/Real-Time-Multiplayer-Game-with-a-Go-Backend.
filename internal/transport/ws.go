// Package transport adapts WebSocket connections to the hub.
//
// It owns everything socket-shaped — handshake, deadlines, heartbeats, framing
// — so that the hub and game packages stay free of network concerns and remain
// testable without opening a port.
package transport

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yaseensharaf/game-arena/internal/hub"
)

const sessionCookie = "arena_session"

// Options configures the WebSocket handler.
type Options struct {
	// AllowedOrigins lists origins permitted to open a socket. Empty means
	// same-origin only, which is the safe default: a wildcard would let any
	// website on the internet open authenticated sockets against this server
	// on a visitor's behalf.
	AllowedOrigins []string
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	PingInterval   time.Duration
	MaxMessageSize int64
}

// Handler bridges HTTP requests to hub sessions.
type Handler struct {
	hub      *hub.Hub
	log      *slog.Logger
	opts     Options
	upgrader websocket.Upgrader

	// live counts the read and write goroutines of every open session.
	// Shutdown waits on it so that the final frames — including the shutdown
	// notice itself — are actually flushed to the socket before the process
	// exits. Without this the server tells players it is going away and then
	// dies before the message leaves the buffer.
	live sync.WaitGroup
}

// NewHandler builds a WebSocket handler.
func NewHandler(h *hub.Hub, log *slog.Logger, opts Options) *Handler {
	handler := &Handler{hub: h, log: log, opts: opts}
	handler.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     handler.checkOrigin,
	}
	return handler
}

// checkOrigin implements the same-origin policy for the WebSocket handshake.
// Browsers do not apply CORS to WebSockets, so without this check any page
// could open a socket against this server using the visitor's cookies.
func (h *Handler) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser client (curl, a test, another service). There is no
		// cookie-riding risk without a browser, so allow it.
		return true
	}

	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	for _, allowed := range h.opts.AllowedOrigins {
		if strings.EqualFold(allowed, origin) || strings.EqualFold(allowed, u.Host) {
			return true
		}
	}

	// Default: same host as the request.
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}

	h.log.Warn("rejected websocket handshake", "origin", origin, "host", r.Host)
	return false
}

// ServeHTTP upgrades the connection and runs the session.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session := h.sessionFrom(r)

	// The cookie is set before the upgrade, because headers cannot be written
	// once the connection has been hijacked for WebSocket framing.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    string(session),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		h.log.Debug("upgrade failed", "err", err)
		return
	}

	client := h.hub.NewClient(session)
	log := h.log.With("session", shortID(string(session)))

	// Two goroutines per session: this one (read) and the write loop.
	h.live.Add(2)
	go func() {
		defer h.live.Done()
		h.writeLoop(conn, client, log)
	}()
	defer h.live.Done()

	h.hub.Join(client)
	h.readLoop(conn, client, log)
}

// Wait blocks until every open session has finished, or the timeout elapses.
// It reports whether all sessions drained in time, so the caller can log a
// truncated shutdown rather than pretending it was clean.
func (h *Handler) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.live.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// sessionFrom recovers an existing session so a reconnecting player returns to
// their seat. The query parameter exists for non-browser clients and tests;
// browsers use the cookie.
func (h *Handler) sessionFrom(r *http.Request) hub.SessionID {
	if v := r.URL.Query().Get("session"); isPlausibleSession(v) {
		return hub.SessionID(v)
	}
	if c, err := r.Cookie(sessionCookie); err == nil && isPlausibleSession(c.Value) {
		return hub.SessionID(c.Value)
	}
	return hub.NewSessionID()
}

// isPlausibleSession rejects malformed values before they reach the hub, so a
// crafted value cannot be used to probe the session map with arbitrary input.
func isPlausibleSession(v string) bool {
	if len(v) != 32 {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

type inbound struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

// readLoop consumes client messages until the connection fails.
//
// The read deadline plus pong handler is what makes dead connections
// detectable: a client that vanishes without a TCP FIN (laptop lid closed,
// wifi dropped) would otherwise hold its seat and goroutines indefinitely.
func (h *Handler) readLoop(conn *websocket.Conn, client *hub.Client, log *slog.Logger) {
	defer func() {
		h.hub.Leave(client)
		conn.Close()
	}()

	conn.SetReadLimit(h.opts.MaxMessageSize)
	_ = conn.SetReadDeadline(time.Now().Add(h.opts.ReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(h.opts.ReadTimeout))
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Debug("read closed", "err", err)
			}
			return
		}

		var msg inbound
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Debug("discarding malformed frame", "err", err)
			continue
		}

		switch msg.Type {
		case hub.MsgMove:
			h.hub.Move(client, msg.Index)
		case hub.MsgRematch:
			h.hub.Rematch(client)
		default:
			log.Debug("unknown message type", "type", msg.Type)
		}
	}
}

// writeLoop is the only goroutine that writes to the socket. Concurrent writes
// to a websocket.Conn are undefined behaviour, so funnelling every write —
// including pings — through here is what keeps framing intact.
func (h *Handler) writeLoop(conn *websocket.Conn, client *hub.Client, log *slog.Logger) {
	ticker := time.NewTicker(h.opts.PingInterval)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case payload, ok := <-client.Out():
			_ = conn.SetWriteDeadline(time.Now().Add(h.opts.WriteTimeout))
			if !ok {
				// Hub closed the client: send a clean close frame so the peer
				// knows this was deliberate rather than a network fault.
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				if !errors.Is(err, websocket.ErrCloseSent) {
					log.Debug("write failed", "err", err)
				}
				return
			}

		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(h.opts.WriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
