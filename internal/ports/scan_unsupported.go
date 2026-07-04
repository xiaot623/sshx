//go:build !linux

package ports

func ScanLoopbackListening() ([]int, error) {
	return nil, ErrUnsupported
}
