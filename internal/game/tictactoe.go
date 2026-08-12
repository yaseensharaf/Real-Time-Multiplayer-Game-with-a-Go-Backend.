// Package game holds the rules of tic-tac-toe and nothing else.
//
// It has no knowledge of networking, players, or sessions. That isolation is
// deliberate: the rules are the part most worth testing, and keeping them free
// of I/O means they can be tested exhaustively without a server running.
package game

import "errors"

// Mark identifies who occupies a cell.
type Mark uint8

const (
	Empty Mark = iota
	X
	O
)

func (m Mark) String() string {
	switch m {
	case X:
		return "X"
	case O:
		return "O"
	default:
		return ""
	}
}

// Opponent returns the other player. Opponent(Empty) is Empty.
func (m Mark) Opponent() Mark {
	switch m {
	case X:
		return O
	case O:
		return X
	default:
		return Empty
	}
}

// Outcome describes how a match ended, or that it hasn't.
type Outcome uint8

const (
	InProgress Outcome = iota
	WinX
	WinO
	Draw
)

func (o Outcome) String() string {
	switch o {
	case WinX:
		return "win_x"
	case WinO:
		return "win_o"
	case Draw:
		return "draw"
	default:
		return "in_progress"
	}
}

// Terminal reports whether the match is over.
func (o Outcome) Terminal() bool { return o != InProgress }

const (
	Size  = 3
	Cells = Size * Size
)

// Move rejection reasons. Callers compare with errors.Is; the transport layer
// maps these to messages the player actually sees, so a rejected move gets an
// explanation instead of silence.
var (
	ErrOutOfBounds = errors.New("cell index out of bounds")
	ErrNotYourTurn = errors.New("not your turn")
	ErrCellTaken   = errors.New("cell already taken")
	ErrGameOver    = errors.New("game already finished")
)

var winLines = [8][3]int{
	{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, // rows
	{0, 3, 6}, {1, 4, 7}, {2, 5, 8}, // columns
	{0, 4, 8}, {2, 4, 6}, // diagonals
}

// State is the authoritative state of one match.
//
// State is not safe for concurrent use. In this server every State is owned by
// exactly one goroutine (its Room), so no locking is needed here — see
// internal/hub for how that ownership is enforced.
type State struct {
	Board   [Cells]Mark `json:"board"`
	Turn    Mark        `json:"turn"`
	Outcome Outcome     `json:"outcome"`
	// WinLine holds the three winning indices, or nil if there's no winner.
	// The client uses it to highlight the line rather than recomputing the
	// rules itself — the server stays the only place that knows how to win.
	WinLine []int `json:"winLine,omitempty"`

	moves int
}

// New returns a fresh match with X to move.
func New() *State {
	return &State{Turn: X}
}

// Reset returns the state to a fresh match, keeping the same allocation.
// Used for rematches so a room can be reused without reallocating.
func (s *State) Reset() {
	s.Board = [Cells]Mark{}
	s.Turn = X
	s.Outcome = InProgress
	s.WinLine = nil
	s.moves = 0
}

// ApplyMove validates and applies a move, returning an error describing why it
// was rejected. Validation lives here rather than in the transport layer so
// that every path into the game — network, tests, a future AI opponent — is
// held to the same rules.
func (s *State) ApplyMove(player Mark, index int) error {
	if s.Outcome.Terminal() {
		return ErrGameOver
	}
	if index < 0 || index >= Cells {
		return ErrOutOfBounds
	}
	if player != s.Turn {
		return ErrNotYourTurn
	}
	if s.Board[index] != Empty {
		return ErrCellTaken
	}

	s.Board[index] = player
	s.moves++

	if line, won := s.winningLine(player); won {
		s.WinLine = line
		if player == X {
			s.Outcome = WinX
		} else {
			s.Outcome = WinO
		}
		return nil
	}

	if s.moves == Cells {
		s.Outcome = Draw
		return nil
	}

	s.Turn = player.Opponent()
	return nil
}

func (s *State) winningLine(player Mark) ([]int, bool) {
	for _, line := range winLines {
		if s.Board[line[0]] == player &&
			s.Board[line[1]] == player &&
			s.Board[line[2]] == player {
			return []int{line[0], line[1], line[2]}, true
		}
	}
	return nil, false
}

// Clone returns a deep copy. The room snapshots state this way before handing
// it to another goroutine for serialisation, so the room can keep mutating the
// original without a data race.
func (s *State) Clone() State {
	c := *s
	if s.WinLine != nil {
		c.WinLine = append([]int(nil), s.WinLine...)
	}
	return c
}
