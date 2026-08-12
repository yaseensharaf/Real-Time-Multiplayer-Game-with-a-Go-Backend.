// Package metrics exposes server counters in Prometheus text exposition
// format.
//
// This is written against the format spec rather than pulling in the official
// client library: the surface needed here is a handful of counters and one
// histogram, and keeping the dependency list at exactly one direct dependency
// (gorilla/websocket) is worth more on a small service than the extra features
// would be. If this grew labels or exemplars, the official client would be the
// right call — that tradeoff is noted in the README.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Registry holds all server metrics. Every method is safe for concurrent use.
type Registry struct {
	mu sync.Mutex

	playersConnected    uint64
	playersDisconnected uint64
	playersReconnected  uint64
	roomsOpened         uint64
	roomsClosed         uint64
	movesRejected       uint64
	matchOutcomes       map[string]uint64

	// Match duration histogram, in seconds.
	durationBuckets []float64
	durationCounts  []uint64
	durationSum     float64
	durationTotal   uint64

	// gauges is a hook for live values the hub owns, read at scrape time.
	liveRooms   func() int
	liveWaiting func() int
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		matchOutcomes:   make(map[string]uint64),
		durationBuckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
		durationCounts:  make([]uint64, 8),
		liveRooms:       func() int { return 0 },
		liveWaiting:     func() int { return 0 },
	}
}

// SetGauges wires live occupancy readings, sampled when metrics are scraped
// rather than pushed, so the hub isn't doing work on every state change.
func (r *Registry) SetGauges(rooms, waiting func() int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liveRooms, r.liveWaiting = rooms, waiting
}

func (r *Registry) PlayerConnected() {
	r.mu.Lock()
	r.playersConnected++
	r.mu.Unlock()
}

func (r *Registry) PlayerDisconnected() {
	r.mu.Lock()
	r.playersDisconnected++
	r.mu.Unlock()
}

func (r *Registry) PlayerReconnected() {
	r.mu.Lock()
	r.playersReconnected++
	r.mu.Unlock()
}

func (r *Registry) RoomOpened() {
	r.mu.Lock()
	r.roomsOpened++
	r.mu.Unlock()
}

func (r *Registry) RoomClosed(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roomsClosed++
	secs := d.Seconds()
	r.durationSum += secs
	r.durationTotal++
	for i, b := range r.durationBuckets {
		if secs <= b {
			r.durationCounts[i]++
		}
	}
}

func (r *Registry) MatchFinished(outcome string) {
	r.mu.Lock()
	r.matchOutcomes[outcome]++
	r.mu.Unlock()
}

func (r *Registry) MoveRejected() {
	r.mu.Lock()
	r.movesRejected++
	r.mu.Unlock()
}

// Handler serves the metrics in Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.write(w)
	})
}

// snapshot is a lock-free copy of the registry's values, taken under the lock
// and rendered outside it. Copying Registry itself would copy its mutex, which
// go vet correctly rejects.
type snapshot struct {
	playersConnected    uint64
	playersDisconnected uint64
	playersReconnected  uint64
	roomsOpened         uint64
	roomsClosed         uint64
	movesRejected       uint64
	outcomes            map[string]uint64
	buckets             []float64
	counts              []uint64
	durationSum         float64
	durationTotal       uint64
	rooms               int
	waiting             int
}

func (r *Registry) snapshot() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	outcomes := make(map[string]uint64, len(r.matchOutcomes))
	for k, v := range r.matchOutcomes {
		outcomes[k] = v
	}

	return snapshot{
		playersConnected:    r.playersConnected,
		playersDisconnected: r.playersDisconnected,
		playersReconnected:  r.playersReconnected,
		roomsOpened:         r.roomsOpened,
		roomsClosed:         r.roomsClosed,
		movesRejected:       r.movesRejected,
		outcomes:            outcomes,
		buckets:             append([]float64(nil), r.durationBuckets...),
		counts:              append([]uint64(nil), r.durationCounts...),
		durationSum:         r.durationSum,
		durationTotal:       r.durationTotal,
		rooms:               r.liveRooms(),
		waiting:             r.liveWaiting(),
	}
}

func (r *Registry) write(w http.ResponseWriter) {
	s := r.snapshot()
	outcomes := s.outcomes
	counts := s.counts
	rooms, waiting := s.rooms, s.waiting

	counter := func(name, help string, v uint64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	gauge := func(name, help string, v int) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}

	counter("gamearena_players_connected_total", "Player connections accepted.", s.playersConnected)
	counter("gamearena_players_disconnected_total", "Player connections closed.", s.playersDisconnected)
	counter("gamearena_players_reconnected_total", "Players who resumed a match after dropping.", s.playersReconnected)
	counter("gamearena_rooms_opened_total", "Matches started.", s.roomsOpened)
	counter("gamearena_rooms_closed_total", "Matches ended.", s.roomsClosed)
	counter("gamearena_moves_rejected_total", "Moves rejected by the rules engine.", s.movesRejected)

	gauge("gamearena_rooms_active", "Matches currently in progress.", rooms)
	gauge("gamearena_players_waiting", "Players queued for an opponent.", waiting)

	fmt.Fprintf(w, "# HELP gamearena_match_outcomes_total Matches by final outcome.\n")
	fmt.Fprintf(w, "# TYPE gamearena_match_outcomes_total counter\n")
	keys := make([]string, 0, len(outcomes))
	for k := range outcomes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "gamearena_match_outcomes_total{outcome=%q} %d\n", k, outcomes[k])
	}

	fmt.Fprintf(w, "# HELP gamearena_match_duration_seconds Match duration.\n")
	fmt.Fprintf(w, "# TYPE gamearena_match_duration_seconds histogram\n")
	for i, b := range s.buckets {
		fmt.Fprintf(w, "gamearena_match_duration_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(b, 'g', -1, 64), counts[i])
	}
	fmt.Fprintf(w, "gamearena_match_duration_seconds_bucket{le=\"+Inf\"} %d\n", s.durationTotal)
	fmt.Fprintf(w, "gamearena_match_duration_seconds_sum %g\n", s.durationSum)
	fmt.Fprintf(w, "gamearena_match_duration_seconds_count %d\n", s.durationTotal)
}
