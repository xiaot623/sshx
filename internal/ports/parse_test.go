package ports

import (
	"reflect"
	"testing"
)

func TestParseProcNetTCPLoopbackAndWildcardListenPorts(t *testing.T) {
	data := `
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000:2382 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   2: 0100007F:2383 00000000:0000 01 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   3: C0A80105:2384 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   4: 0100007F:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	got, err := parseProcNetTCP(data, false)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1:8080 and 0.0.0.0:9090 are forwarded; 127.0.0.1:9091 is ESTABLISHED
	// (not LISTEN), 192.168.1.5:9092 is bound to another interface, and 22 is a
	// privileged port — all three excluded.
	if want := []int{8080, 9090}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %#v, want %#v", got, want)
	}
}

func TestParseProcNetTCP6LoopbackAndWildcardListenPorts(t *testing.T) {
	data := `
  sl  local_address                         rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F91 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1F92 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	got, err := parseProcNetTCP(data, true)
	if err != nil {
		t.Fatal(err)
	}
	// ::1:8081 (loopback, host-endian words) and :::8082 (wildcard) are both forwarded.
	if want := []int{8081, 8082}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %#v, want %#v", got, want)
	}
}

func TestParseProcNetTCP6NetworkOrderLoopbackIsNotForwardable(t *testing.T) {
	// Network-order ::1 (...00000001) is what a naive hex→net.IP decode would
	// treat as loopback. The kernel prints host-endian words, so this must not
	// match — otherwise tests pass on a fixture that never appears in /proc.
	data := `
  sl  local_address                         rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000001:1F91 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	got, err := parseProcNetTCP(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ports = %#v, want none", got)
	}
}

func TestConnectHost(t *testing.T) {
	tests := []struct {
		hex  string
		ipv6 bool
		want string
	}{
		{"0100007F", false, "127.0.0.1"},
		{"00000000", false, "127.0.0.1"},
		{"0200007F", false, "127.0.0.1"}, // 127.0.0.2, 127/8
		{"C0A80105", false, ""},
		{"00000000000000000000000001000000", true, "::1"},
		{"00000000000000000000000000000000", true, "127.0.0.1"},
		{"00000000000000000000000000000001", true, ""},
	}
	for _, tt := range tests {
		if got := ConnectHost(tt.hex, tt.ipv6); got != tt.want {
			t.Errorf("ConnectHost(%q, ipv6=%v) = %q, want %q", tt.hex, tt.ipv6, got, tt.want)
		}
	}
}

func TestMergeListenersPrefersIPv4ConnectHost(t *testing.T) {
	v4 := `
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	v6 := `
  sl  local_address                         rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	tcp4, err := parseProcNetTCPListeners(v4, false)
	if err != nil {
		t.Fatal(err)
	}
	tcp6, err := parseProcNetTCPListeners(v6, true)
	if err != nil {
		t.Fatal(err)
	}
	got := mergeListeners(tcp6, tcp4)
	want := []Listener{{Port: 8080, ConnectHost: "127.0.0.1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge v6 then v4 = %#v, want %#v", got, want)
	}
	got = mergeListeners(tcp4, tcp6)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge v4 then v6 = %#v, want %#v", got, want)
	}
}
