package transport

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yaseensharaf/game-arena/internal/hub"
)

func newTestServer(t *testing.T, origins []string) (*httptest.Server, *hub.Hub) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(hub.Config{GracePeriod: 5 * time.Second, SendBuffer: 32}, log, hub.NopMetrics{})
	handler := NewHandler(h, log, Options{
		AllowedOrigins: origins,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   5 * time.Second,
		PingInterval:   time.Second,
		MaxMessageSize: 1024,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, h
}

func wsURL(srv *httptest.Server, query string) string {
	u := "ws" + strings.TrimPrefix(srv.URL, "http")
	if query != "" {
		u += "?" + query
	}
	return u
}

func dial(t *testing.T, srv *httptest.Server, query string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, query), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type frame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func readUntil(t *testing.T, c *websocket.Conn, want string) frame {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < 20; i++ {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read while awaiting %q: %v", want, err)
		}
		var f frame
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		if f.Type == want {
			return f
		}
	}
	t.Fatalf("never received %q", want)
	return frame{}
}

// TestFullMatchOverWebSocket exercises the real network path: handshake,
// matchmaking, moves, and win detection, with nothing stubbed.
func TestFullMatchOverWebSocket(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	a := dial(t, srv, "")
	b := dial(t, srv, "")

	var assigned struct {
		Mark    string `json:"mark"`
		Session string `json:"session"`
		Room    string `json:"room"`
	}
	f := readUntil(t, a, hub.MsgAssigned)
	if err := json.Unmarshal(f.Data, &assigned); err != nil {
		t.Fatalf("unmarshal assigned: %v", err)
	}
	if assigned.Mark != "X" {
		t.Fatalf("first player mark = %q, want X", assigned.Mark)
	}
	if len(assigned.Session) != 32 {
		t.Fatalf("session = %q, want 32 hex chars", assigned.Session)
	}
	readUntil(t, b, hub.MsgAssigned)

	// Moves must be synchronised on observed state, not on write order. The
	// two sockets are independent, so writing a then b back to back does not
	// guarantee the server sees them in that order — and the server is right
	// to reject the out-of-turn one. Waiting for each move to land is what
	// makes this test deterministic rather than flaky.
	var final boardState
	for _, mv := range []struct {
		c *websocket.Conn
		i int
	}{{a, 0}, {b, 3}, {a, 1}, {b, 4}, {a, 2}} {
		if err := mv.c.WriteJSON(map[string]any{"type": "move", "index": mv.i}); err != nil {
			t.Fatalf("write move: %v", err)
		}
		final = awaitBoard(t, a, func(s boardState) bool {
			return s.Board[mv.i] != 0
		})
	}

	if final.Outcome != 1 { // WinX
		t.Fatalf("outcome = %d, want 1 (WinX)", final.Outcome)
	}
	if len(final.WinLine) != 3 {
		t.Fatalf("winLine = %v, want 3 indices for the UI to highlight", final.WinLine)
	}
}

type boardState struct {
	Board   [9]int `json:"board"`
	Turn    int    `json:"turn"`
	Outcome int    `json:"outcome"`
	WinLine []int  `json:"winLine"`
}

// awaitBoard reads state frames until one satisfies cond, so tests wait for a
// condition to hold rather than for a fixed duration.
func awaitBoard(t *testing.T, c *websocket.Conn, cond func(boardState) bool) boardState {
	t.Helper()
	for i := 0; i < 20; i++ {
		f := readUntil(t, c, hub.MsgState)
		var s boardState
		if err := json.Unmarshal(f.Data, &s); err != nil {
			t.Fatalf("unmarshal state: %v", err)
		}
		if cond(s) {
			return s
		}
	}
	t.Fatal("condition never satisfied")
	return boardState{}
}

// TestReconnectOverWebSocket verifies a player can drop their socket entirely
// and resume the same match using their session, which is the whole point of
// the grace period.
func TestReconnectOverWebSocket(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	a := dial(t, srv, "")
	b := dial(t, srv, "")

	var assigned struct {
		Session string `json:"session"`
		Mark    string `json:"mark"`
	}
	json.Unmarshal(readUntil(t, a, hub.MsgAssigned).Data, &assigned)
	readUntil(t, b, hub.MsgAssigned)

	a.WriteJSON(map[string]any{"type": "move", "index": 4})
	readUntil(t, b, hub.MsgState)

	// Hard drop, no close handshake — simulates a network failure.
	a.Close()
	readUntil(t, b, hub.MsgOpponentDisconnected)

	a2 := dial(t, srv, "session="+assigned.Session)
	var back struct {
		Mark string `json:"mark"`
	}
	json.Unmarshal(readUntil(t, a2, hub.MsgAssigned).Data, &back)
	if back.Mark != assigned.Mark {
		t.Fatalf("mark after reconnect = %q, want %q", back.Mark, assigned.Mark)
	}

	var state struct {
		Board []int `json:"board"`
	}
	json.Unmarshal(readUntil(t, a2, hub.MsgState).Data, &state)
	if len(state.Board) != 9 || state.Board[4] != 1 {
		t.Fatalf("board not restored on reconnect: %v", state.Board)
	}
	readUntil(t, b, hub.MsgOpponentReconnected)
}

// TestCrossOriginHandshakeRejected covers the security fix: browsers do not
// apply CORS to WebSockets, so the server must reject foreign origins itself.
func TestCrossOriginHandshakeRejected(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	header := http.Header{}
	header.Set("Origin", "https://evil.example")

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, ""), header)
	if err == nil {
		t.Fatal("handshake from a foreign origin was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

func TestConfiguredOriginAccepted(t *testing.T) {
	srv, _ := newTestServer(t, []string{"https://trusted.example"})

	header := http.Header{}
	header.Set("Origin", "https://trusted.example")

	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, ""), header)
	if err != nil {
		t.Fatalf("allow-listed origin rejected: %v", err)
	}
	defer c.Close()
	readUntil(t, c, hub.MsgWaiting)
}

func TestMalformedFramesIgnored(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	a := dial(t, srv, "")
	b := dial(t, srv, "")
	readUntil(t, a, hub.MsgAssigned)
	readUntil(t, b, hub.MsgAssigned)

	// Garbage must not kill the connection.
	a.WriteMessage(websocket.TextMessage, []byte("{not json"))
	a.WriteMessage(websocket.TextMessage, []byte(`{"type":"nonsense"}`))
	a.WriteJSON(map[string]any{"type": "move", "index": 4})

	// b's first state frame is the empty starting board, so wait for the one
	// that reflects the move rather than asserting on whatever arrives first.
	state := awaitBoard(t, b, func(s boardState) bool { return s.Board[4] != 0 })
	if state.Board[4] != 1 {
		t.Fatalf("connection did not survive malformed frames: %v", state.Board)
	}
}

func TestInvalidSessionParamIgnored(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	// A non-hex session must be discarded rather than used as a map key.
	c := dial(t, srv, "session=../../etc/passwd")
	readUntil(t, c, hub.MsgWaiting)
}
