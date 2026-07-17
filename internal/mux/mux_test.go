package mux

import (
	"io"
	"net"
	"testing"
)

func TestChannelsAreIndependent(t *testing.T) {
	a, b := net.Pipe()
	left := New(a)
	right := New(b)
	defer left.Close()
	defer right.Close()

	go func() {
		_, _ = left.Channel(ChannelControl).Write([]byte("control"))
		_, _ = left.Channel(ChannelFS).Write([]byte("filesystem"))
	}()
	control := make([]byte, len("control"))
	if _, err := io.ReadFull(right.Channel(ChannelControl), control); err != nil || string(control) != "control" {
		t.Fatalf("control = %q, %v", control, err)
	}
	fs := make([]byte, len("filesystem"))
	if _, err := io.ReadFull(right.Channel(ChannelFS), fs); err != nil || string(fs) != "filesystem" {
		t.Fatalf("fs = %q, %v", fs, err)
	}
}
