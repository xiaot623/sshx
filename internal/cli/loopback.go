package cli

import (
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// loopbackPoolSize is how many 127.64.0.x loopback aliases sshx pre-provisions on
// macOS so that per-target forwarding listeners (allocated by locald starting at
// 127.64.0.1) can bind without an EADDRNOTAVAIL failure. Linux treats the whole
// 127.0.0.0/8 as local and needs no aliases, so this is darwin-only.
const loopbackPoolSize = 64

// loopbackAliasBase matches the second octet used by locald's allocator.
const loopbackAliasBase = 64

var loopbackAliasRE = regexp.MustCompile(`(?m)^\s*inet\s+127\.64\.0\.(\d+)\s`)

// missingLoopbackAliases returns the 127.64.0.x addresses (full "ip:port-less"
// form, i.e. just the IP) from the pool [1..poolSize] that are NOT present in
// ifconfigOutput. Pure function for testability.
func missingLoopbackAliases(ifconfigOutput string, poolSize int) []string {
	present := make(map[string]bool, poolSize)
	for _, m := range loopbackAliasRE.FindAllStringSubmatch(ifconfigOutput, -1) {
		present["127.64.0."+m[1]] = true
	}
	var missing []string
	for i := 1; i <= poolSize; i++ {
		ip := "127.64.0." + strconv.Itoa(i)
		if !present[ip] {
			missing = append(missing, ip)
		}
	}
	return missing
}

// loopbackAliasCommands returns the "ifconfig lo0 alias <ip> up" shell commands
// needed to provision the darwin loopback pool, or nil if nothing is missing or
// the platform is not darwin.
func loopbackAliasCommands() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("ifconfig", "lo0").Output()
	if err != nil {
		// Best-effort: assume nothing present, provision the whole pool.
		out = nil
	}
	missing := missingLoopbackAliases(string(out), loopbackPoolSize)
	if len(missing) == 0 {
		return nil
	}
	cmds := make([]string, 0, len(missing))
	for _, ip := range missing {
		cmds = append(cmds, "ifconfig lo0 alias "+ip+" up")
	}
	return cmds
}

// joinShellCommands joins independent shell commands with " ; " so a single
// sudo invocation runs them all; a failure of one does not abort the others
// (we want best-effort provisioning of the whole pool).
func joinShellCommands(cmds []string) string {
	return strings.Join(cmds, " ; ")
}