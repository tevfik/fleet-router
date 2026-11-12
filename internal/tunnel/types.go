package tunnel

import "time"

// MessageType defines the framing protocol over the WebSocket tunnel.
type MessageType string

const (
	MsgRegister MessageType = "register"
	MsgPing     MessageType = "ping"
	MsgPong     MessageType = "pong"
	MsgRequest  MessageType = "request"
	MsgResponse MessageType = "response"
	MsgChunk    MessageType = "chunk"
	MsgClose    MessageType = "close"
	// MsgStreamOpen (hub → node) asks the node to open a raw TCP stream: ID is the stream id the node
	// presents on /fleet/tunnel/stream/accept, Path is "127.0.0.1:<port>". The node refuses with a
	// MsgClose carrying the same ID and an Error. Nodes that predate streams ignore it (they only act on
	// MsgRequest), and the hub never sends it to a node that declared no StreamPorts.
	MsgStreamOpen MessageType = "stream_open"
)

// Message carries multiplexed HTTP requests and agent registrations over WebSocket.
type Message struct {
	Type       MessageType       `json:"type"`
	ID         string            `json:"id,omitempty"`
	Method     string            `json:"method,omitempty"`
	Path       string            `json:"path,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	StatusCode int               `json:"status_code,omitempty"`
	Body       []byte            `json:"body,omitempty"`
	NodeInfo   *NodeRegistration `json:"node_info,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// NodeRegistration is sent on tunnel connect to describe the joined device.
type NodeRegistration struct {
	NodeName  string    `json:"node_name"`
	GPU       string    `json:"gpu,omitempty"`
	VRAMGB    float64   `json:"vram_gb,omitempty"`
	RAMGB     float64   `json:"ram_gb,omitempty"`
	Models    []string  `json:"models,omitempty"`
	JoinedAt  time.Time `json:"joined_at"`
	AuthToken string    `json:"auth_token,omitempty"`
	// FleetVersion / EngineVersion are what the node runs (empty from a node that predates them).
	FleetVersion  string `json:"fleet_version,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`
	// StreamPorts are the local TCP ports the node lets the hub open raw streams to (fleet join
	// --stream-port). The hub refuses any other port; an empty list means no streams at all.
	StreamPorts []int `json:"stream_ports,omitempty"`
}

// AllowsStreamPort reports whether the node declared port as streamable.
func (r NodeRegistration) AllowsStreamPort(port int) bool {
	for _, p := range r.StreamPorts {
		if p == port && p > 0 && p < 65536 {
			return true
		}
	}
	return false
}
