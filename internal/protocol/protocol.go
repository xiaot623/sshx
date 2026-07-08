package protocol

import (
	"bufio"
	"encoding/json"
	"io"
)

const Version = 1

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
)

const (
	RoleClient    = "client"
	RoleRequester = "requester"
)

type Frame struct {
	Type            string            `json:"type"`
	ID              string            `json:"id,omitempty"`
	ProtocolVersion int               `json:"protocolVersion,omitempty"`
	Role            string            `json:"role,omitempty"`
	Token           string            `json:"token,omitempty"`
	Capabilities    []string          `json:"capabilities,omitempty"`
	Argv            []string          `json:"argv,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	Stdin           string            `json:"stdin,omitempty"`
	ExitCode        int               `json:"exitCode,omitempty"`
	Stdout          string            `json:"stdout,omitempty"`
	Stderr          string            `json:"stderr,omitempty"`
	Error           string            `json:"error,omitempty"`
	Port            int               `json:"port,omitempty"`
	Host            string            `json:"host,omitempty"`
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
