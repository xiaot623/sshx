//go:build linux

package ports

import "os"

func ScanLoopbackListeners() ([]Listener, error) {
	tcp4, err := scanProcFile("/proc/net/tcp", false)
	if err != nil {
		return nil, err
	}
	tcp6, err := scanProcFile("/proc/net/tcp6", true)
	if err != nil {
		return nil, err
	}
	return mergeListeners(tcp4, tcp6), nil
}

func scanProcFile(path string, ipv6 bool) ([]Listener, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseProcNetTCPListeners(string(b), ipv6)
}
