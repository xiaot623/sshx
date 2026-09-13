package mux

import (
	"io"
	"net"
	"testing"
)

func TestChannelsAreIndependent(t *testing.T) {
	a, b := net.Pipe()
	serverErr := make(chan error, 1)
	var server *Session
	go func() {
		s, err := NewServer(b)
		server = s
		serverErr <- err
	}()
	client, err := NewClient(a)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
	defer server.Close()

	go func() {
		_, _ = client.Channel(ChannelControl).Write([]byte("control"))
		_, _ = client.Channel(ChannelFS).Write([]byte("filesystem"))
	}()
	control := make([]byte, len("control"))
	if _, err := io.ReadFull(server.Channel(ChannelControl), control); err != nil || string(control) != "control" {
		t.Fatalf("control = %q, %v", control, err)
	}
	fs := make([]byte, len("filesystem"))
	if _, err := io.ReadFull(server.Channel(ChannelFS), fs); err != nil || string(fs) != "filesystem" {
		t.Fatalf("fs = %q, %v", fs, err)
	}
}
