# game-arena

Real-time multiplayer tic-tac-toe with a server-authoritative Go backend.

The game is small on purpose. The interesting part is everything around it:
concurrent match state without locks, connections that survive a dropped
network, and the observability needed to tell what a running server is doing.

```
┌──────────┐   WebSocket    ┌─────────────────────────────────┐
│ Browser  │◄──────────────►│  Handler (deadlines, heartbeat) │
│ (canvas) │                └───────────────┬─────────────────┘
└──────────┘                                │
                                    ┌───────▼────────┐
                                    │      Hub       │  matchmaking
                                    │  (mutex: maps) │  session routing
                                    └───────┬────────┘
                                            │ channels
                          ┌─────────────────▼──────────────────┐
                          │  Room goroutine (one per match)    │
                          │  sole owner of game.State          │
                          └────────────────────────────────────┘
```

## Run it

```bash
go run ./cmd/server        # then open http://localhost:8080 in two tabs
make race                  # full test suite under the race detector
docker compose up --build  # containerised
```

Two tabs are matched against each other automatically. To see reconnect work,
kill the network on one tab (DevTools → Network → Offline) for a few seconds
and turn it back on: the board comes back exactly as it was.

## Design decisions

**Game state is owned by one goroutine, not guarded by a lock.** Each match
runs its own `Room` goroutine, and every interaction — moves, joins, departures,
rematch requests — arrives on a channel. `game.State` therefore has no mutex,
because nothing else can reach it. Concurrency bugs in that layer are excluded
by construction rather than by remembering to lock. The cost is a channel round
trip per message, which is irrelevant at the message rate a board game produces.
Under a workload of thousands of messages per second per match, sharded locks
would be the better trade.

**The hub's mutex guards bookkeeping maps only, and is never held across a
channel send.** Holding it during a send would deadlock against a room
goroutine calling back into `onRoomClosed` as it exits. This constraint is
written down in the package comment because it is the kind of rule that gets
broken by a well-meaning later edit.

**The client sends intent; the server decides everything.** There is no message
a client can send that asserts a board state or a winner — only "I want cell 4".
A modified client can send illegal moves all day and will get an error back with
a reason. Validation lives in `game`, not in the transport, so every path in
(network, tests, a future AI opponent) is held to identical rules.

**Slow clients are dropped from, not waited on.** Each connection has a buffered
outbound channel; if it fills, the message is discarded and logged. The
alternative — blocking the room goroutine on a send — would let one stalled
player freeze their opponent's game. Dropping a frame is recoverable because
every state message is a full snapshot rather than a delta.

**Disconnects hold the seat instead of ending the match.** A dropped player has
30 seconds (configurable) to return with their session ID before the match is
abandoned. Sessions come from `crypto/rand`: a guessable session would let
someone take over another player's seat.

**Metrics are hand-rolled against the Prometheus text format.** The surface
needed here is six counters, two gauges, and one histogram, and the project has
exactly one direct dependency (`gorilla/websocket`) as a result. If this grew
labels or exemplars, the official client library would be the right call — the
tradeoff is deliberate, not an oversight.

## Failure handling

| Failure | Behaviour |
|---|---|
| Player closes the tab | Seat held 30s, opponent notified, match abandoned on expiry |
| Network drops silently (laptop lid, wifi loss) | Ping/pong with a read deadline detects it within ~60s |
| Player stops reading messages | Frames dropped for that client only; opponent unaffected |
| Illegal or out-of-turn move | Rejected with a reason sent back to the mover |
| Malformed or oversized frame | Discarded; connection survives; reads capped at 1 KiB |
| Cross-origin handshake | Rejected — browsers do not apply CORS to WebSockets |
| `SIGTERM` | Players notified, rooms drained, writes flushed, then exit |

## Testing

```
internal/game       79.6%    internal/hub         87.7%
internal/config     86.5%    internal/transport   81.1%
internal/metrics    91.8%
```

Everything runs under `-race` in CI. Three things are worth calling out:

**The rules are verified exhaustively, not by example.** A DFS walks every
reachable position and asserts invariants at each one: both players can never
hold a winning line, the recorded outcome always matches the board, mark counts
stay balanced, and no move is ever accepted after a terminal state. It explores
all 255,168 legal games — a number that matches the known value for tic-tac-toe,
which is itself evidence the engine is right.

**Leaks have named regression tests.** `TestRoomsCleanedUp` and
`TestClientChannelClosedOnLeave` exist because both of those leaked in the first
version of this server. A leak that is fixed but untested comes back.

**Transport tests run against a real server.** `httptest` plus real WebSocket
dials, including a hard connection drop followed by a session reconnect, so the
handshake, deadlines, and framing are all exercised rather than stubbed.

## Configuration

All settings are environment variables, validated at startup — the server
refuses to boot on a bad combination rather than misbehaving later. For
instance, a `PING_INTERVAL` at or above `READ_TIMEOUT` would disconnect healthy
players on a timer, so it is rejected by name.

| Variable | Default | Purpose |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `ALLOWED_ORIGINS` | *(same-origin)* | Comma-separated origins allowed to connect |
| `GRACE_PERIOD` | `30s` | How long a seat is held for a dropped player |
| `READ_TIMEOUT` | `60s` | Connection considered dead without a pong |
| `PING_INTERVAL` | `25s` | Heartbeat interval; must be below `READ_TIMEOUT` |
| `SHUTDOWN_TIMEOUT` | `10s` | Budget for draining on `SIGTERM` |
| `SEND_BUFFER` | `16` | Per-client outbound queue depth |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `text` | Use `json` in deployment |

## Endpoints

| Path | Purpose |
|---|---|
| `/ws` | WebSocket upgrade |
| `/healthz` | Liveness |
| `/readyz` | Readiness, with room and queue counts |
| `/metrics` | Prometheus exposition format |

## Protocol

Server → client: `assigned`, `state`, `waiting`, `error`,
`opponent_disconnected`, `opponent_reconnected`, `opponent_left`,
`rematch_offered`, `server_shutdown`.

Client → server: `move` (with an index), `rematch`.

Every `state` frame is a complete snapshot including the winning line, so a
client that missed a frame recovers on the next one without replaying history.

## Known limitations

Room state lives in memory, so matches do not survive a restart and the server
does not scale past one instance. Moving rooms behind Redis and routing by room
ID is the natural next step, and is the change this design was shaped to allow:
the room's inputs are already a message channel rather than direct method calls.

Matchmaking is a single queue with no rating or region awareness. There is no
authentication — a session identifies a seat, not a person.

## Stack

Go 1.22 (goroutines, channels, `log/slog`, `net/http`), `gorilla/websocket`,
vanilla JS with no framework. Game logic is hand-rolled rather than pulled from
a library, since demonstrating the concurrency model is the point.
