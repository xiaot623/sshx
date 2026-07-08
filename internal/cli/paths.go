package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiaot623/sshx/internal/locald"
)

func domainSuffix() string {
	return defaultDomainSuffix()
}

func domainDNSAddr() string {
	if v := os.Getenv("SSHX_DOMAIN_DNS_ADDR"); v != "" {
		return v
	}
	return defaultDomainDNSAddr()
}

func defaultDomainDNSAddr() string {
	return "127.0.0.1:53535"
}

func defaultDomainSuffix() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "user"
	}
	return user + ".sshx"
}

func splitHostPortDefault(addr, defaultPort string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		return host, port, nil
	}
	if strings.Count(addr, ":") == 0 {
		return addr, defaultPort, nil
	}
	return "", "", err
}

func defaultSocketPath() string {
	if serverHome := os.Getenv("SSHX_SERVER_HOME"); serverHome != "" {
		return filepath.Join(serverHome, "sock")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx.sock")
	}
	return filepath.Join(home, ".sshx", "sock")
}

func defaultLocalDaemonSocketPath() string {
	if override := os.Getenv("SSHX_LOCAL_DAEMON_SOCKET"); override != "" {
		return override
	}
	return locald.DefaultSocketPath()
}

func defaultCacheRoot() string {
	if override := os.Getenv("SSHX_CACHE_DIR"); override != "" {
		return override
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "sshx")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-cache")
	}
	return filepath.Join(home, ".cache", "sshx")
}

func defaultVersionStatePath() string {
	if serverHome := os.Getenv("SSHX_SERVER_HOME"); serverHome != "" {
		return filepath.Join(serverHome, "version-state.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-version-state.json")
	}
	return filepath.Join(home, ".sshx", "version-state.json")
}

func defaultRemoteHostsPath() string {
	if override := os.Getenv("SSHX_REMOTE_HOSTS"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-remote-hosts.json")
	}
	return filepath.Join(home, ".sshx", "remote-hosts.json")
}

func defaultServerInfoPath() string {
	if serverHome := os.Getenv("SSHX_SERVER_HOME"); serverHome != "" {
		return filepath.Join(serverHome, "server-info")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-server-info")
	}
	return filepath.Join(home, ".sshx", "server-info")
}

func IsReservedLocalError(s string) bool {
	return strings.Contains(s, reservedLocalMessage)
}
