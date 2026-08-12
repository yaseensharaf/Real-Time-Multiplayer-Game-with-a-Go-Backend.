package game

import (
	"errors"
	"testing"
)

// play applies a sequence of moves, alternating starting with X, and fails the
// test if any move is unexpectedly rejected.
func play(t *testing.T, s *State, indices ...int) {
	t.Helper()
	turn := X
	for i, idx := range indices {
		if err := s.ApplyMove(turn, idx); err != nil {
			t.Fatalf("move %d (%s at %d) rejected: %v", i, turn, idx, err)
		}
		turn = turn.Opponent()
	}
}

func TestNewStateStartsWithX(t *testing.T) {
	s := New()
	if s.Turn != X {
		t.Errorf("Turn = %v, want X", s.Turn)
	}
	if s.Outcome != InProgress {
		t.Errorf("Outcome = %v, want InProgress", s.Outcome)
	}
	for i, c := range s.Board {
		if c != Empty {
			t.Errorf("Board[%d] = %v, want Empty", i, c)
		}
	}
}

func TestAllWinLinesDetected(t *testing.T) {
	for _, line := range winLines {
		t.Run("", func(t *testing.T) {
			s := New()
			// X takes the win line; O plays filler cells that never win.
			filler := fillerCells(line)
			for i := 0; i < 3; i++ {
				if err := s.ApplyMove(X, line[i]); err != nil {
					t.Fatalf("X move %d: %v", line[i], err)
				}
				if i < 2 {
					if err := s.ApplyMove(O, filler[i]); err != nil {
						t.Fatalf("O move %d: %v", filler[i], err)
					}
				}
			}
			if s.Outcome != WinX {
				t.Fatalf("Outcome = %v, want WinX for line %v", s.Outcome, line)
			}
			if len(s.WinLine) != 3 {
				t.Fatalf("WinLine = %v, want 3 indices", s.WinLine)
			}
		})
	}
}

// fillerCells returns cells not on the winning line, so O's moves can't
// accidentally win or block.
func fillerCells(exclude [3]int) []int {
	var out []int
	for i := 0; i < Cells; i++ {
		if i != exclude[0] && i != exclude[1] && i != exclude[2] {
			out = append(out, i)
		}
	}
	return out
}

func TestDraw(t *testing.T) {
	s := New()
	// X O X
	// X O O
	// O X X  -> full board, no winner
	play(t, s, 0, 1, 2, 4, 3, 5, 7, 6, 8)
	if s.Outcome != Draw {
		t.Fatalf("Outcome = %v, want Draw", s.Outcome)
	}
	if s.WinLine != nil {
		t.Errorf("WinLine = %v, want nil on a draw", s.WinLine)
	}
}

func TestRejections(t *testing.T) {
	tests := []struct {
		name    string
		setup   []int // moves alternating from X
		player  Mark
		index   int
		wantErr error
	}{
		{"out of turn", []int{0}, X, 4, ErrNotYourTurn},
		{"o moves first", nil, O, 0, ErrNotYourTurn},
		{"cell taken", []int{4}, O, 4, ErrCellTaken},
		{"index too high", nil, X, Cells, ErrOutOfBounds},
		{"index negative", nil, X, -1, ErrOutOfBounds},
		{"empty mark cannot move", nil, Empty, 0, ErrNotYourTurn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New()
			play(t, s, tt.setup...)
			err := s.ApplyMove(tt.player, tt.index)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestMovesAfterGameOverRejected(t *testing.T) {
	s := New()
	play(t, s, 0, 3, 1, 4, 2) // X wins top row
	if s.Outcome != WinX {
		t.Fatalf("setup failed: outcome = %v", s.Outcome)
	}
	if err := s.ApplyMove(O, 5); !errors.Is(err, ErrGameOver) {
		t.Fatalf("err = %v, want ErrGameOver", err)
	}
}

func TestRejectedMoveLeavesStateUntouched(t *testing.T) {
	s := New()
	play(t, s, 4)
	before := s.Clone()

	_ = s.ApplyMove(X, 0) // out of turn
	_ = s.ApplyMove(O, 4) // taken
	_ = s.ApplyMove(O, 99)

	if s.Board != before.Board || s.Turn != before.Turn || s.Outcome != before.Outcome {
		t.Fatal("rejected moves mutated state")
	}
}

func TestReset(t *testing.T) {
	s := New()
	play(t, s, 0, 3, 1, 4, 2)
	s.Reset()
	if s.Turn != X || s.Outcome != InProgress || s.WinLine != nil || s.moves != 0 {
		t.Fatalf("Reset left dirty state: %+v", s)
	}
	for i, c := range s.Board {
		if c != Empty {
			t.Fatalf("Board[%d] = %v after Reset", i, c)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := New()
	play(t, s, 0, 3, 1, 4, 2) // produces a WinLine
	c := s.Clone()
	c.WinLine[0] = 99
	c.Board[8] = O
	if s.WinLine[0] == 99 {
		t.Error("Clone shares WinLine backing array")
	}
	if s.Board[8] == O {
		t.Error("Clone shares board")
	}
}

// TestExhaustiveInvariants walks every reachable game via DFS and asserts the
// engine can never reach a contradictory state. This is the test that actually
// earns confidence: it covers all 255,168 legal games rather than the handful
// a human would think to write by hand.
func TestExhaustiveInvariants(t *testing.T) {
	var games, terminals int

	var walk func(s *State)
	walk = func(s *State) {
		games++

		// Invariant: at most one player can occupy a winning line.
		xWins := hasLine(s, X)
		oWins := hasLine(s, O)
		if xWins && oWins {
			t.Fatalf("both players hold a winning line: %v", s.Board)
		}

		// Invariant: the recorded outcome matches the board.
		switch {
		case xWins && s.Outcome != WinX:
			t.Fatalf("X has a line but outcome = %v", s.Outcome)
		case oWins && s.Outcome != WinO:
			t.Fatalf("O has a line but outcome = %v", s.Outcome)
		}

		// Invariant: X and O counts differ by 0 or 1, X never behind.
		nx, no := count(s, X), count(s, O)
		if d := nx - no; d != 0 && d != 1 {
			t.Fatalf("mark counts out of balance: X=%d O=%d", nx, no)
		}

		if s.Outcome.Terminal() {
			terminals++
			// Invariant: no move is accepted once terminal.
			for i := 0; i < Cells; i++ {
				if err := s.ApplyMove(s.Turn, i); !errors.Is(err, ErrGameOver) {
					t.Fatalf("terminal state accepted move %d: %v", i, err)
				}
			}
			return
		}

		for i := 0; i < Cells; i++ {
			if s.Board[i] != Empty {
				continue
			}
			next := s.Clone()
			if err := next.ApplyMove(next.Turn, i); err != nil {
				t.Fatalf("legal move %d rejected: %v", i, err)
			}
			walk(&next)
		}
	}

	walk(New())

	if games == 0 || terminals == 0 {
		t.Fatalf("walk did not explore: games=%d terminals=%d", games, terminals)
	}
	t.Logf("explored %d states, %d terminal", games, terminals)
}

func hasLine(s *State, m Mark) bool {
	_, ok := s.winningLine(m)
	return ok
}

func count(s *State, m Mark) int {
	n := 0
	for _, c := range s.Board {
		if c == m {
			n++
		}
	}
	return n
}

func BenchmarkApplyMove(b *testing.B) {
	for i := 0; i < b.N; i++ {
		s := New()
		s.ApplyMove(X, 0)
		s.ApplyMove(O, 3)
		s.ApplyMove(X, 1)
		s.ApplyMove(O, 4)
		s.ApplyMove(X, 2)
	}
}
