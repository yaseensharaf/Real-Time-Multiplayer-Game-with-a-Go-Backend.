package hub

// The wire protocol. Every frame is a JSON object with a "type" discriminator
// and an optional "data" payload.
//
// The client sends intent ("I want to play cell 4") and never state. The
// server replies with authoritative state. This split is what stops a modified
// client from declaring itself the winner: there is no message that says so.
const (
	// Server -> client.
	MsgAssigned             = "assigned"              // your mark, session, room
	MsgState                = "state"                 // authoritative board
	MsgWaiting              = "waiting"               // queued for an opponent
	MsgError                = "error"                 // your last action was rejected
	MsgOpponentDisconnected = "opponent_disconnected" // seat held, reconnect window open
	MsgOpponentReconnected  = "opponent_reconnected"
	MsgOpponentLeft         = "opponent_left" // match over, seat released
	MsgRematchOffered       = "rematch_offered"
	MsgServerShutdown       = "server_shutdown"

	// Client -> server.
	MsgMove    = "move"
	MsgRematch = "rematch"
)

type message struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

type assignedPayload struct {
	Mark    string `json:"mark"`
	Session string `json:"session"`
	Room    string `json:"room"`
}

type errorPayload struct {
	Reason string `json:"reason"`
}

type disconnectPayload struct {
	GraceSeconds int `json:"graceSeconds"`
}

type leftPayload struct {
	Reason string `json:"reason"`
}

type rematchPayload struct {
	By string `json:"by"`
}
