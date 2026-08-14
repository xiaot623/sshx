//go:build !linux

package ports

func ScanLoopbackListeners() ([]Listener, error) {
	return nil, ErrUnsupported
}
