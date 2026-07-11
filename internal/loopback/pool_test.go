package loopback

import "testing"

func TestAddressBoundaries(t *testing.T) {
	tests := []struct {
		index int
		want  string
	}{
		{index: -1, want: ""},
		{index: 0, want: "127.64.0.1"},
		{index: Size - 1, want: "127.64.0.64"},
		{index: Size, want: ""},
	}
	for _, test := range tests {
		if got := Address(test.index); got != test.want {
			t.Errorf("Address(%d) = %q, want %q", test.index, got, test.want)
		}
	}
}
