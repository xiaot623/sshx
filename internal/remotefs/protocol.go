package remotefs

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	frameHello    = "hello"
	frameHelloOK  = "hello.ok"
	frameRequest  = "request"
	frameResponse = "response"
	frameCancel   = "cancel"
)

var ErrFrameTooLarge = errors.New("remote fs frame is too large")

type wireFrame struct {
	Type      string     `json:"type"`
	Version   int        `json:"version,omitempty"`
	ID        uint64     `json:"id,omitempty"`
	SessionID string     `json:"sessionId,omitempty"`
	Token     string     `json:"token,omitempty"`
	MountID   string     `json:"mountId,omitempty"`
	Op        string     `json:"op,omitempty"`
	Path      string     `json:"path,omitempty"`
	Path2     string     `json:"path2,omitempty"`
	Target    string     `json:"target,omitempty"`
	Handle    uint64     `json:"handle,omitempty"`
	Offset    int64      `json:"offset,omitempty"`
	Size      uint32     `json:"size,omitempty"`
	OpenFlags OpenFlags  `json:"openFlags,omitempty"`
	Mode      uint32     `json:"mode,omitempty"`
	Attr      Attr       `json:"attr,omitempty"`
	Change    SetAttr    `json:"change,omitempty"`
	Entries   []DirEntry `json:"entries,omitempty"`
	StatFS    StatFS     `json:"statfs,omitempty"`
	MountPath string     `json:"mountPath,omitempty"`
	ReadOnly  bool       `json:"readOnly,omitempty"`
	ErrorCode ErrorCode  `json:"errorCode,omitempty"`
	Error     string     `json:"error,omitempty"`
	Data      []byte     `json:"-"`
}

func writeWireFrame(w io.Writer, frame wireFrame) error {
	metadata, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(metadata) > MaxMetadataSize {
		return fmt.Errorf("%w: metadata %d bytes", ErrFrameTooLarge, len(metadata))
	}
	if len(frame.Data) > MaxDataSize {
		return fmt.Errorf("%w: data %d bytes", ErrFrameTooLarge, len(frame.Data))
	}
	var lengths [8]byte
	binary.BigEndian.PutUint32(lengths[0:4], uint32(len(metadata)))
	binary.BigEndian.PutUint32(lengths[4:8], uint32(len(frame.Data)))
	if err := writeAll(w, lengths[:]); err != nil {
		return err
	}
	if err := writeAll(w, metadata); err != nil {
		return err
	}
	if len(frame.Data) > 0 {
		err = writeAll(w, frame.Data)
	}
	return err
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readWireFrame(r io.Reader) (wireFrame, error) {
	var lengths [8]byte
	if _, err := io.ReadFull(r, lengths[:]); err != nil {
		return wireFrame{}, err
	}
	metadataSize := binary.BigEndian.Uint32(lengths[0:4])
	dataSize := binary.BigEndian.Uint32(lengths[4:8])
	if metadataSize == 0 || metadataSize > MaxMetadataSize {
		return wireFrame{}, errors.New("invalid remote fs metadata frame size")
	}
	if dataSize > MaxDataSize {
		return wireFrame{}, errors.New("invalid remote fs data frame size")
	}
	metadata := make([]byte, metadataSize)
	if _, err := io.ReadFull(r, metadata); err != nil {
		return wireFrame{}, err
	}
	var frame wireFrame
	if err := json.Unmarshal(metadata, &frame); err != nil {
		return wireFrame{}, err
	}
	if dataSize > 0 {
		frame.Data = make([]byte, dataSize)
		if _, err := io.ReadFull(r, frame.Data); err != nil {
			return wireFrame{}, err
		}
	}
	return frame, nil
}
