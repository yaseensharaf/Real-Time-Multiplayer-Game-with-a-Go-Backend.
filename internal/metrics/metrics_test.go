package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestCountersAppearInOutput(t *testing.T) {
	r := New()
	r.PlayerConnected()
	r.PlayerConnected()
	r.RoomOpened()
	r.MatchFinished("win_x")
	r.MoveRejected()

	body := scrape(t, r)

	for _, want := range []string{
		"gamearena_players_connected_total 2",
		"gamearena_rooms_opened_total 1",
		`gamearena_match_outcomes_total{outcome="win_x"} 1`,
		"gamearena_moves_rejected_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q\n---\n%s", want, body)
		}
	}
}

// TestExpositionFormat checks the output is structurally valid Prometheus
// text: every metric needs HELP and TYPE lines, or scrapers reject it.
func TestExpositionFormat(t *testing.T) {
	r := New()
	r.RoomClosed(3 * time.Second)
	body := scrape(t, r)

	var help, typ int
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP"):
			help++
		case strings.HasPrefix(line, "# TYPE"):
			typ++
		}
	}
	if help == 0 || help != typ {
		t.Fatalf("HELP lines = %d, TYPE lines = %d; every metric needs both", help, typ)
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := New()
	r.RoomClosed(2 * time.Second)   // <= 5
	r.RoomClosed(45 * time.Second)  // <= 60
	r.RoomClosed(400 * time.Second) // <= 600

	body := scrape(t, r)

	// A 2s match falls in every bucket from 5 upward, so le="60" must count
	// at least the 2s and 45s matches.
	if !strings.Contains(body, `gamearena_match_duration_seconds_bucket{le="60"} 2`) {
		t.Errorf("le=60 bucket wrong\n---\n%s", body)
	}
	if !strings.Contains(body, "gamearena_match_duration_seconds_count 3") {
		t.Errorf("count wrong\n---\n%s", body)
	}
}

func TestGaugesReadLiveValues(t *testing.T) {
	r := New()
	rooms, waiting := 7, 3
	r.SetGauges(func() int { return rooms }, func() int { return waiting })

	if body := scrape(t, r); !strings.Contains(body, "gamearena_rooms_active 7") {
		t.Errorf("gauge not reflected\n---\n%s", body)
	}

	rooms = 2
	if body := scrape(t, r); !strings.Contains(body, "gamearena_rooms_active 2") {
		t.Errorf("gauge is not sampled at scrape time\n---\n%s", body)
	}
}

// TestConcurrentUpdates must be run with -race; the registry is written from
// every room goroutine and read by the scrape handler at the same time.
func TestConcurrentUpdates(t *testing.T) {
	r := New()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.PlayerConnected()
			r.RoomOpened()
			r.MatchFinished("draw")
			r.RoomClosed(time.Second)
			r.MoveRejected()
		}()
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		}()
	}
	wg.Wait()

	if body := scrape(t, r); !strings.Contains(body, "gamearena_players_connected_total 50") {
		t.Errorf("lost updates under concurrency\n---\n%s", body)
	}
}
