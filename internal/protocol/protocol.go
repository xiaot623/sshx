package protocol

import (
	"bufio"
	"encoding/json"
	"io"
)

const (
	Version    = 5
	MinVersion = 5
	MaxVersion = 5
)

const (
	TypeHello         = "hello"
	TypeCapabilities  = "capabilities"
	TypeCommandExec   = "command.exec"
	TypeCommandResult = "command.result"
	TypeCommandError  = "command.error"
	TypePortObserved  = "port.observed"
	TypePortGone      = "port.gone"
	TypePortForward   = "port.forward"
	TypeError         = "error"
	TypeHeartbeat     = "heartbeat"
	TypeHeartbeatAck  = "heartbeat.ack"
	TypeServerDrain   = "server.drain"
)

const (
	RoleClient    = "client"
	RoleRequester = "requester"
)

type Frame struct {
	Type            string            `json:"type"`
	ID              string            `json:"id,omitempty"`
	ProtocolVersion int               `json:"protocolVersion,omitempty"`
	ProtocolMin     int               `json:"protocolMin,omitempty"`
	ProtocolMax     int               `json:"protocolMax,omitempty"`
	RuntimeID       string            `json:"runtimeId,omitempty"`
	AppVersion      string            `json:"appVersion,omitempty"`
	TargetID        string            `json:"targetId,omitempty"`
	ContextID       string            `json:"contextId,omitempty"`
	SessionID       string            `json:"sessionId,omitempty"`
	RequestID       string            `json:"requestId,omitempty"`
	Sequence        uint64            `json:"sequence,omitempty"`
	Role            string            `json:"role,omitempty"`
	Token           string            `json:"token,omitempty"`
	Capabilities    []string          `json:"capabilities,omitempty"`
	Argv            []string          `json:"argv,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	RemoteFS        bool              `json:"remoteFs,omitempty"`
	MountID         string            `json:"mountId,omitempty"`
	MountPath       string            `json:"mountPath,omitempty"`
	MountReadOnly   bool              `json:"mountReadOnly,omitempty"`
	Stdin           string            `json:"stdin,omitempty"`
	TimeoutMillis   int64             `json:"timeoutMillis,omitempty"`
	ExitCode        int               `json:"exitCode,omitempty"`
	Stdout          string            `json:"stdout,omitempty"`
	Stderr          string            `json:"stderr,omitempty"`
	Error           string            `json:"error,omitempty"`
	Port            int               `json:"port,omitempty"`
	Host            string            `json:"host,omitempty"`
}

func Compatible(minVersion, maxVersion int) bool {
	if minVersion == 0 {
		minVersion = Version
	}
	if maxVersion == 0 {
		maxVersion = Version
	}
	return minVersion <= MaxVersion && maxVersion >= MinVersion
}

func FrameCompatible(frame Frame) bool {
	if frame.ProtocolMin == 0 && frame.ProtocolMax == 0 && frame.ProtocolVersion == 0 {
		return false
	}
	minVersion, maxVersion := frame.ProtocolMin, frame.ProtocolMax
	if minVersion == 0 {
		minVersion = frame.ProtocolVersion
	}
	if maxVersion == 0 {
		maxVersion = frame.ProtocolVersion
	}
	return Compatible(minVersion, maxVersion)
}

type Encoder struct {
	enc *json.Encoder
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{enc: json.NewEncoder(w)}
}

func (e *Encoder) Encode(f Frame) error {
	return e.enc.Encode(f)
}

type Decoder struct {
	dec *json.Decoder
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{dec: json.NewDecoder(bufio.NewReader(r))}
}

func (d *Decoder) Decode() (Frame, error) {
	var f Frame
	err := d.dec.Decode(&f)
	return f, err
}
