package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yaseensharaf/game-arena/internal/game"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testHub(t *testing.T, grace time.Duration) *Hub {
	t.Helper()
	return New(Config{GracePeriod: grace, SendBuffer: 32}, testLogger(), NopMetrics{})
}

// recv reads the next message for a client, failing the test on timeout so a
// missing message shows up as a clear failure rather than a hung test.
func recv(t *testing.T, c *Client) message {
	t.Helper()
	select {
	case payload, ok := <-c.Out():
		if !ok {
			t.Fatal("client channel closed while awaiting message")
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return message{}
	}
}

// waitFor reads until a message of the given type arrives, skipping others.
func waitFor(t *testing.T, c *Client, msgType string) message {
	t.Helper()
	for i := 0; i < 20; i++ {
		m := recv(t, c)
		if m.Type == msgType {
			return m
		}
	}
	t.Fatalf("never received %q", msgType)
	return message{}
}

// decodeState pulls the game state out of a state message.
func decodeState(t *testing.T, m message) game.State {
	t.Helper()
	raw, err := json.Marshal(m.Data)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	var s game.State
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return s
}

func decodeAssigned(t *testing.T, m message) assignedPayload {
	t.Helper()
	raw, _ := json.Marshal(m.Data)
	var a assignedPayload
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("unmarshal assigned: %v", err)
	}
	return a
}

func TestFirstPlayerWaits(t *testing.T) {
	h := testHub(t, time.Second)
	c := h.NewClient("")
	h.Join(c)

	if m := recv(t, c); m.Type != MsgWaiting {
		t.Fatalf("type = %q, want %q", m.Type, MsgWaiting)
	}
	if rooms, waiting := h.Stats(); rooms != 0 || waiting != 1 {
		t.Fatalf("stats = (%d, %d), want (0, 1)", rooms, waiting)
	}
}

func TestTwoPlayersMatched(t *testing.T) {
	h := testHub(t, time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)

	am := decodeAssigned(t, waitFor(t, a, MsgAssigned))
	bm := decodeAssigned(t, waitFor(t, b, MsgAssigned))

	if am.Mark != "X" || bm.Mark != "O" {
		t.Fatalf("marks = %q/%q, want X/O", am.Mark, bm.Mark)
	}
	if am.Room != bm.Room || am.Room == "" {
		t.Fatalf("rooms = %q/%q, want equal and non-empty", am.Room, bm.Room)
	}
	if rooms, _ := h.Stats(); rooms != 1 {
		t.Fatalf("rooms = %d, want 1", rooms)
	}
}

// TestPlayerLearnsItsMark guards the bug where a client could never tell
// whether it was X or O, making the board unplayable without guessing.
func TestPlayerLearnsItsMark(t *testing.T) {
	h := testHub(t, time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)

	if got := decodeAssigned(t, waitFor(t, a, MsgAssigned)).Mark; got != "X" {
		t.Fatalf("first player mark = %q, want X", got)
	}
	if got := decodeAssigned(t, waitFor(t, b, MsgAssigned)).Mark; got != "O" {
		t.Fatalf("second player mark = %q, want O", got)
	}
}

func TestMoveBroadcastToBothPlayers(t *testing.T) {
	h := testHub(t, time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)
	waitFor(t, b, MsgState)

	h.Move(a, 4)

	sa := decodeState(t, waitFor(t, a, MsgState))
	sb := decodeState(t, waitFor(t, b, MsgState))
	if sa.Board[4] != game.X || sb.Board[4] != game.X {
		t.Fatalf("boards not synced: %v / %v", sa.Board, sb.Board)
	}
	if sa.Turn != game.O {
		t.Fatalf("turn = %v, want O", sa.Turn)
	}
}

func TestOutOfTurnMoveRejectedWithReason(t *testing.T) {
	h := testHub(t, time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)
	waitFor(t, b, MsgState)

	h.Move(b, 0) // O tries to move first

	m := waitFor(t, b, MsgError)
	raw, _ := json.Marshal(m.Data)
	var e errorPayload
	json.Unmarshal(raw, &e)
	if e.Reason != game.ErrNotYourTurn.Error() {
		t.Fatalf("reason = %q, want %q", e.Reason, game.ErrNotYourTurn.Error())
	}
}

func TestWinBroadcastsOutcome(t *testing.T) {
	h := testHub(t, time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)
	waitFor(t, b, MsgState)

	// X: 0,1,2   O: 3,4
	for _, mv := range []struct {
		c *Client
		i int
	}{{a, 0}, {b, 3}, {a, 1}, {b, 4}, {a, 2}} {
		h.Move(mv.c, mv.i)
		time.Sleep(10 * time.Millisecond)
	}

	drainUntilOutcome := func(c *Client) game.Outcome {
		for i := 0; i < 30; i++ {
			m := recv(t, c)
			if m.Type == MsgState {
				if s := decodeState(t, m); s.Outcome.Terminal() {
					return s.Outcome
				}
			}
		}
		t.Fatal("no terminal state received")
		return game.InProgress
	}

	if got := drainUntilOutcome(a); got != game.WinX {
		t.Fatalf("outcome = %v, want WinX", got)
	}
}

// TestReconnectRestoresSeat is the core of the disconnect-tolerance feature:
// a dropped player rejoining with the same session must get their board back
// rather than being dumped into a new match.
func TestReconnectRestoresSeat(t *testing.T) {
	h := testHub(t, 5*time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	session := a.Session
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)
	waitFor(t, b, MsgState)

	h.Move(a, 4)
	waitFor(t, b, MsgState)

	// A drops.
	h.Leave(a)
	waitFor(t, b, MsgOpponentDisconnected)

	// A returns with the same session.
	a2 := h.NewClient(session)
	h.Join(a2)

	assigned := decodeAssigned(t, waitFor(t, a2, MsgAssigned))
	if assigned.Mark != "X" {
		t.Fatalf("mark after reconnect = %q, want X", assigned.Mark)
	}
	state := decodeState(t, waitFor(t, a2, MsgState))
	if state.Board[4] != game.X {
		t.Fatalf("board not restored: %v", state.Board)
	}
	waitFor(t, b, MsgOpponentReconnected)

	if rooms, _ := h.Stats(); rooms != 1 {
		t.Fatalf("rooms = %d, want 1 (should not have created a new match)", rooms)
	}
}

func TestReconnectAfterGraceExpiryFails(t *testing.T) {
	h := testHub(t, 50*time.Millisecond)
	a, b := h.NewClient(""), h.NewClient("")
	session := a.Session
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)
	waitFor(t, b, MsgState)

	h.Leave(a)
	waitFor(t, b, MsgOpponentLeft)

	// Room should be gone, so the session no longer resolves.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if rooms, _ := h.Stats(); rooms == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rooms, _ := h.Stats(); rooms != 0 {
		t.Fatalf("rooms = %d, want 0 after grace expiry", rooms)
	}

	a2 := h.NewClient(session)
	h.Join(a2)
	if m := recv(t, a2); m.Type != MsgWaiting {
		t.Fatalf("type = %q, want %q (stale session should start fresh)", m.Type, MsgWaiting)
	}
}

func TestRematchRequiresBothPlayers(t *testing.T) {
	h := testHub(t, time.Second)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)
	waitFor(t, b, MsgState)

	for _, mv := range []struct {
		c *Client
		i int
	}{{a, 0}, {b, 3}, {a, 1}, {b, 4}, {a, 2}} {
		h.Move(mv.c, mv.i)
		time.Sleep(10 * time.Millisecond)
	}

	// One-sided request must not reset the board.
	h.Rematch(a)
	waitFor(t, b, MsgRematchOffered)
	time.Sleep(50 * time.Millisecond)

	h.Rematch(b)
	// Both agreed: marks swap, so A is now O.
	assigned := decodeAssigned(t, waitFor(t, a, MsgAssigned))
	if assigned.Mark != "O" {
		t.Fatalf("mark after rematch = %q, want O (marks should alternate)", assigned.Mark)
	}
	state := decodeState(t, waitFor(t, a, MsgState))
	if state.Outcome != game.InProgress {
		t.Fatalf("outcome = %v, want InProgress", state.Outcome)
	}
	for i, c := range state.Board {
		if c != game.Empty {
			t.Fatalf("board[%d] = %v, want empty after rematch", i, c)
		}
	}
}

// TestRoomsCleanedUp guards the leak where finished rooms stayed in the hub's
// maps forever, growing memory without bound on a long-running server.
func TestRoomsCleanedUp(t *testing.T) {
	h := testHub(t, 10*time.Millisecond)

	for i := 0; i < 20; i++ {
		a, b := h.NewClient(""), h.NewClient("")
		h.Join(a)
		h.Join(b)
		h.Leave(a)
		h.Leave(b)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rooms, _ := h.Stats(); rooms == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rooms, _ := h.Stats()
	if rooms != 0 {
		t.Fatalf("rooms = %d, want 0 — rooms are leaking", rooms)
	}

	h.mu.Lock()
	nClients, nSessions := len(h.byClient), len(h.bySession)
	h.mu.Unlock()
	if nClients != 0 || nSessions != 0 {
		t.Fatalf("index maps leaked: byClient=%d bySession=%d", nClients, nSessions)
	}
}

// TestClientChannelClosedOnLeave guards the goroutine leak where a transport
// write loop ranged over a channel that was never closed.
func TestClientChannelClosedOnLeave(t *testing.T) {
	h := testHub(t, 10*time.Millisecond)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)

	h.Leave(a)
	h.Leave(b)

	for _, c := range []*Client{a, b} {
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("client was never closed — transport goroutine would leak")
		}
	}
}

// TestConcurrentPlayersNoRace hammers the hub from many goroutines. Run with
// -race, this is what catches unsynchronised access to the maps or state.
func TestConcurrentPlayersNoRace(t *testing.T) {
	h := testHub(t, 50*time.Millisecond)

	const pairs = 40
	var wg sync.WaitGroup
	for i := 0; i < pairs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, b := h.NewClient(""), h.NewClient("")
			h.Join(a)
			h.Join(b)

			// Drain concurrently so buffers never stall the room.
			for _, c := range []*Client{a, b} {
				go func(c *Client) {
					for range c.Out() {
					}
				}(c)
			}

			for i := 0; i < 9; i++ {
				h.Move(a, i)
				h.Move(b, i)
			}
			h.Rematch(a)
			h.Rematch(b)
			h.Leave(a)
			h.Leave(b)
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rooms, _ := h.Stats(); rooms == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	rooms, _ := h.Stats()
	t.Fatalf("rooms = %d, want 0 after all players left", rooms)
}

func TestShutdownNotifiesAndClosesRooms(t *testing.T) {
	h := testHub(t, time.Minute)
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)
	waitFor(t, a, MsgState)

	done := make(chan struct{})
	go func() {
		h.Shutdown(2 * time.Second)
		close(done)
	}()

	waitFor(t, a, MsgServerShutdown)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if rooms, _ := h.Stats(); rooms != 0 {
		t.Fatalf("rooms = %d, want 0 after shutdown", rooms)
	}
}

func TestJoinAfterShutdownRejected(t *testing.T) {
	h := testHub(t, time.Second)
	h.Shutdown(time.Second)

	c := h.NewClient("")
	h.Join(c)
	if m := recv(t, c); m.Type != MsgServerShutdown {
		t.Fatalf("type = %q, want %q", m.Type, MsgServerShutdown)
	}
}

// TestSlowClientDoesNotBlockRoom verifies the drop-on-full policy: a player who
// stops reading must not be able to freeze their opponent's game.
func TestSlowClientDoesNotBlockRoom(t *testing.T) {
	h := New(Config{GracePeriod: time.Second, SendBuffer: 1}, testLogger(), NopMetrics{})
	a, b := h.NewClient(""), h.NewClient("")
	h.Join(a)
	h.Join(b)

	// b never drains its channel. a should still get responses.
	go func() {
		for range a.Out() {
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			h.Move(a, i%9)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("room blocked on a slow client")
	}
}
