//go:build !linux

package ports

import "errors"

func ScanLoopbackListeners() ([]Listener, error) {
	return nil, errors.New("port scanning is only supported on Linux servers")
}
