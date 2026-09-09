//go:build linux

package ports

import "os"

func ScanLoopbackListeners() ([]Listener, error) {
	tcp4, err := scanProcFile("/proc/net/tcp")
	if err != nil {
		return nil, err
	}
	tcp6, err := scanProcFile("/proc/net/tcp6")
	if err != nil {
		return nil, err
	}
	return mergeListeners(tcp4, tcp6), nil
}

func scanProcFile(path string) ([]Listener, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseProcNetTCPListeners(string(b))
}
