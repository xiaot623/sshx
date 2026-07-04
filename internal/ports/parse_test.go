package ports

import (
	"reflect"
	"testing"
)

func TestParseProcNetTCPOnlyLoopbackListenPorts(t *testing.T) {
	data := `
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000:2382 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   2: 0100007F:2383 00000000:0000 01 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	got, err := parseProcNetTCP(data, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{8080}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %#v, want %#v", got, want)
	}
}

func TestParseProcNetTCP6OnlyLoopbackListenPorts(t *testing.T) {
	data := `
  sl  local_address                         rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000001:1F91 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1F92 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000000000000000 100 0 0 10 0
`
	got, err := parseProcNetTCP(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{8081}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %#v, want %#v", got, want)
	}
}
