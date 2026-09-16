package mux

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/xtaci/smux"
)

const (
	ChannelControl byte = 1
	ChannelFS      byte = 2
)

type Session struct {
	session   *smux.Session
	channels  map[byte]io.ReadWriteCloser
	closeOnce sync.Once
}

func NewClient(transport io.ReadWriteCloser) (*Session, error) {
	session, err := smux.Client(transport, smux.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return openSession(session, true)
}

func NewServer(transport io.ReadWriteCloser) (*Session, error) {
	session, err := smux.Server(transport, smux.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return openSession(session, false)
}

func openSession(smuxSession *smux.Session, client bool) (*Session, error) {
	s := &Session{session: smuxSession, channels: map[byte]io.ReadWriteCloser{}}
	for _, id := range []byte{ChannelControl, ChannelFS} {
		stream, err := nextStream(smuxSession, client)
		if err != nil {
			_ = smuxSession.Close()
			return nil, err
		}
		s.channels[id] = stream
	}
	return s, nil
}

func nextStream(session *smux.Session, client bool) (*smux.Stream, error) {
	if client {
		return session.OpenStream()
	}
	return session.AcceptStream()
}

func (s *Session) Channel(id byte) io.ReadWriteCloser {
	return s.channels[id]
}

func (s *Session) Done() <-chan struct{} { return s.session.CloseChan() }

func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.session.Close()
	})
	return err
}

func Proxy(ctx context.Context, transport io.ReadWriteCloser, control, fs io.ReadWriteCloser) error {
	session, err := NewServer(transport)
	if err != nil {
		return err
	}
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
