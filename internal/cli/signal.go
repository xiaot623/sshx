package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type processSignalCause struct {
	signal os.Signal
}

func (c processSignalCause) Error() string {
	return fmt.Sprintf("received signal %s", c.signal)
}

func (c processSignalCause) processSignal() os.Signal {
	return c.signal
}

// NotifySignalContext preserves the concrete process signal as the context
// cause so transparent adapters can forward it to the real OpenSSH process.
func NotifySignalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	stopped := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		select {
		case received := <-signals:
			cancel(processSignalCause{signal: received})
		case <-stopped:
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(stopped)
			cancel(context.Canceled)
		})
	}
}
