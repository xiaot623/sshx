package mux

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

const (
	ChannelControl byte = 1
	ChannelFS      byte = 2
	maxFrame            = 64 << 10
)

type Session struct {
	transport io.ReadWriteCloser
	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	channels  map[byte]*Channel
}

type Channel struct {
	id        byte
	session   *Session
	r         *io.PipeReader
	w         *io.PipeWriter
	closeOnce sync.Once
}

func New(transport io.ReadWriteCloser) *Session {
	s := &Session{transport: transport, done: make(chan struct{}), channels: map[byte]*Channel{}}
	for _, id := range []byte{ChannelControl, ChannelFS} {
		r, w := io.Pipe()
		s.channels[id] = &Channel{id: id, session: s, r: r, w: w}
	}
	go s.readLoop()
	return s
}

func (s *Session) Channel(id byte) io.ReadWriteCloser {
	return s.channels[id]
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.transport.Close()
		for _, ch := range s.channels {
			_ = ch.w.Close()
			_ = ch.r.Close()
		}
		close(s.done)
	})
	return err
}

func (s *Session) readLoop() {
	defer s.Close()
	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(s.transport, header); err != nil {
			return
		}
		id := header[0] & 0x7f
		closing := header[0]&0x80 != 0
		length := binary.BigEndian.Uint32(header[1:])
		if length > maxFrame {
			return
		}
		channel := s.channels[id]
		if channel == nil {
			if _, err := io.CopyN(io.Discard, s.transport, int64(length)); err != nil {
				return
			}
			continue
		}
		if length > 0 {
			if _, err := io.CopyN(channel.w, s.transport, int64(length)); err != nil {
				return
			}
		}
		if closing {
			_ = channel.w.Close()
		}
	}
}

func (s *Session) write(id byte, p []byte, closing bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	header := make([]byte, 5)
	header[0] = id
	if closing {
		header[0] |= 0x80
	}
	binary.BigEndian.PutUint32(header[1:], uint32(len(p)))
	if _, err := s.transport.Write(header); err != nil {
		return err
	}
	if len(p) > 0 {
		_, err := s.transport.Write(p)
		return err
	}
	return nil
}

func (c *Channel) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *Channel) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxFrame {
			n = maxFrame
		}
		if err := c.session.write(c.id, p[:n], false); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *Channel) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.session.write(c.id, nil, true)
		_ = c.r.Close()
	})
	return err
}

func Proxy(ctx context.Context, transport io.ReadWriteCloser, control, fs io.ReadWriteCloser) error {
	session := New(transport)
	defer session.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 4)
	copyPair := func(channelID byte, target io.ReadWriteCloser) {
		channel := session.Channel(channelID)
		go func() {
			_, err := io.Copy(target, channel)
			errCh <- normalizeCopyError(err)
		}()
		go func() {
			_, err := io.Copy(channel, target)
			_ = channel.Close()
			errCh <- normalizeCopyError(err)
		}()
	}
	copyPair(ChannelControl, control)
	copyPair(ChannelFS, fs)
	select {
	case <-ctx.Done():
		return nil
	case <-session.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func normalizeCopyError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}
