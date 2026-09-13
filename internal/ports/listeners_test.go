package ports

import (
	"net"
	"reflect"
	"testing"
)

func TestListenerFromAddr(t *testing.T) {
	tests := []struct {
		name   string
		ip     net.IP
		port   int
		want   Listener
		wantOK bool
	}{
		{
			name:   "ipv4 loopback",
			ip:     net.ParseIP("127.0.0.1"),
			port:   8080,
			want:   Listener{Port: 8080, ConnectHost: "127.0.0.1"},
			wantOK: true,
		},
		{
			name:   "ipv4 unspecified",
			ip:     net.ParseIP("0.0.0.0"),
			port:   8080,
			want:   Listener{Port: 8080, ConnectHost: "127.0.0.1"},
			wantOK: true,
		},
		{
			name:   "ipv6 loopback",
			ip:     net.ParseIP("::1"),
			port:   8080,
			want:   Listener{Port: 8080, ConnectHost: "::1"},
			wantOK: true,
		},
		{
			name:   "ipv6 unspecified",
			ip:     net.ParseIP("::"),
			port:   8080,
			want:   Listener{Port: 8080, ConnectHost: "127.0.0.1"},
			wantOK: true,
		},
		{
			name:   "non-loopback ipv4 skipped",
			ip:     net.ParseIP("192.168.1.5"),
			port:   8080,
			wantOK: false,
		},
		{
			name:   "privileged port skipped",
			ip:     net.ParseIP("127.0.0.1"),
			port:   22,
			wantOK: false,
		},
		{
			name:   "nil ip skipped",
			port:   8080,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := listenerFromAddr(tt.ip, tt.port)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %#v)", ok, tt.wantOK, got)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("listener = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMergeListenersPrefersIPv4ConnectHost(t *testing.T) {
	v4 := []Listener{{Port: 8080, ConnectHost: "127.0.0.1"}}
	v6 := []Listener{{Port: 8080, ConnectHost: "::1"}}
	want := []Listener{{Port: 8080, ConnectHost: "127.0.0.1"}}

	got := mergeListeners(v6, v4)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge v6 then v4 = %#v, want %#v", got, want)
	}
	got = mergeListeners(v4, v6)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge v4 then v6 = %#v, want %#v", got, want)
	}
}

func TestMergeListenersSortsByPort(t *testing.T) {
	got := mergeListeners(
		[]Listener{{Port: 9090, ConnectHost: "127.0.0.1"}},
		[]Listener{{Port: 8080, ConnectHost: "::1"}},
	)
	want := []Listener{
		{Port: 8080, ConnectHost: "::1"},
		{Port: 9090, ConnectHost: "127.0.0.1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %#v, want %#v", got, want)
	}
}
