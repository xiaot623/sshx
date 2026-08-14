package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	localproxy "github.com/xiaot623/sshx/internal/proxy"
	"github.com/xiaot623/sshx/internal/sshcompat"
)

type proxyEnvironment struct {
	HTTP  string
	SOCKS string
}

func (e proxyEnvironment) script() string {
	if e.HTTP == "" || e.SOCKS == "" {
		return ""
	}
	noProxy := "localhost,127.0.0.1,::1"
	return strings.Join([]string{
		"HTTP_PROXY=" + shellQuote(e.HTTP),
		"HTTPS_PROXY=" + shellQuote(e.HTTP),
		"ALL_PROXY=" + shellQuote(e.SOCKS),
		"http_proxy=$HTTP_PROXY",
		"https_proxy=$HTTPS_PROXY",
		"all_proxy=$ALL_PROXY",
		"NO_PROXY=\"${NO_PROXY:+$NO_PROXY,}${no_proxy:+$no_proxy,}" + noProxy + "\"",
		"no_proxy=\"$NO_PROXY\"",
		"export HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy NO_PROXY no_proxy",
	}, "; ")
}

type proxyTunnel struct {
	server      *localproxy.Server
	controlPath string
	sshArgs     []string
	spec        string
	environment proxyEnvironment
	runner      *Runner
	closeOnce   sync.Once
}

// skipOptionalProxy reports a non-strict proxy setup failure and returns true
// when the enhanced session should continue without proxy. Strict mode returns
// false so the caller can abort.
func (r *Runner) skipOptionalProxy(target string, err error) bool {
	if err == nil {
		return true
	}
	if r.strict {
		return false
	}
	fmt.Fprintf(r.Stderr, "sshx: proxy skipped for %s: %v\n", target, err)
	return true
}

func (r *Runner) startProxyTunnel(ctx context.Context, sshArgs []string, controlPath string) (*proxyTunnel, error) {
	if controlPath == "" {
		return nil, errors.New("proxy requires an OpenSSH control socket")
	}
	server, err := localproxy.Start(ctx, localproxy.Options{Getenv: os.Getenv})
	if err != nil {
		return nil, fmt.Errorf("start local proxy: %w", err)
	}
	_, localPort, err := net.SplitHostPort(server.Address())
	if err != nil {
		_ = server.Close()
		return nil, err
	}
	requestSpec := "127.0.0.1:0:127.0.0.1:" + localPort
	output, err := r.ExecOutput(ctx, r.SSHPath, sshControlOperationArgs(sshArgs, controlPath, "forward", requestSpec))
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("create reverse proxy forwarding: %w", err)
	}
	remotePort, err := parseAllocatedPort(output)
	if err != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = r.ExecOutput(cancelCtx, r.SSHPath, sshControlOperationArgs(sshArgs, controlPath, "cancel", requestSpec))
		cancel()
		_ = server.Close()
		return nil, err
	}
	user := url.UserPassword(server.Username(), server.Password())
	remoteAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort))
	tunnel := &proxyTunnel{
		server:      server,
		controlPath: controlPath,
		sshArgs:     append([]string(nil), sshArgs...),
		// OpenSSH indexes dynamically allocated remote forwards by their
		// original listen port (0), not by the allocated port it reports.
		// The cancel operation must therefore repeat the request verbatim.
		spec: requestSpec,
		environment: proxyEnvironment{
			HTTP:  (&url.URL{Scheme: "http", User: user, Host: remoteAddress}).String(),
			SOCKS: (&url.URL{Scheme: "socks5h", User: user, Host: remoteAddress}).String(),
		},
		runner: r,
	}
	return tunnel, nil
}

func (t *proxyTunnel) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = t.runner.ExecOutput(ctx, t.runner.SSHPath, sshControlOperationArgs(t.sshArgs, t.controlPath, "cancel", t.spec))
		_ = t.server.Close()
	})
}

func parseAllocatedPort(output []byte) (int, error) {
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return 0, errors.New("OpenSSH did not report an allocated proxy port")
	}
	for _, field := range fields {
		port, err := strconv.Atoi(field)
		if err == nil && port > 0 && port <= 65535 {
			return port, nil
		}
	}
	return 0, fmt.Errorf("invalid allocated proxy port %q", strings.TrimSpace(string(output)))
}

func sshControlOperationArgs(sshArgs []string, controlPath, operation, forwardSpec string) []string {
	clean := stripProxyForwardingOptions(stripAuxiliaryActionOptions(stripControlOptions(sshArgs)))
	parsed := sshcompat.Parse(clean)
	options := []string{
		"-S", controlPath,
		"-O", operation,
		"-o", "ExitOnForwardFailure=yes",
		"-R", forwardSpec,
	}
	return insertBeforeTarget(parsed, options)
}

func stripProxyForwardingOptions(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-o" && i+1 < len(args) {
			key, _, _ := strings.Cut(args[i+1], "=")
			if isProxyForwardingOption(key) {
				i++
				continue
			}
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 {
			key, _, _ := strings.Cut(arg[2:], "=")
			if isProxyForwardingOption(key) {
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func isProxyForwardingOption(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "clearallforwardings", "exitonforwardfailure", "localforward", "remoteforward", "dynamicforward":
		return true
	default:
		return false
	}
}

func proxyControlMasterArgs(sshArgs []string, controlPath string) []string {
	clean := stripProxyForwardingOptions(stripAuxiliaryActionOptions(stripControlOptions(sshArgs)))
	parsed := sshcompat.Parse(clean)
	return insertBeforeTarget(parsed, []string{
		"-o", "ControlMaster=yes",
		"-o", "ControlPersist=no",
		"-S", controlPath,
		"-o", "ClearAllForwardings=yes",
	})
}

func waitForControlPath(ctx context.Context, path string) bool {
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return waitForPath(waitCtx, path)
}

func integrationProxyEnvPath(contextHome, sessionID string) string {
	return contextHome + "/proxy/" + sessionID + ".env"
}

func integrationProxyErrorPath(contextHome, sessionID string) string {
	return contextHome + "/proxy/" + sessionID + ".error"
}

func integrationProxyWaitScript(contextHome, sessionID string, strict bool) string {
	envPath := integrationProxyEnvPath(contextHome, sessionID)
	errorPath := integrationProxyErrorPath(contextHome, sessionID)
	strictValue := "0"
	if strict {
		strictValue = "1"
	}
	return strings.Join([]string{
		"proxy_env=\"" + strings.ReplaceAll(envPath, `"`, `\"`) + "\"",
		"proxy_error=\"" + strings.ReplaceAll(errorPath, `"`, `\"`) + "\"",
		"i=0",
		"while [ ! -f \"$proxy_env\" ] && [ ! -f \"$proxy_error\" ] && [ $i -lt 60 ]; do i=$((i+1)); sleep 0.05; done",
		"if [ -f \"$proxy_env\" ]; then . \"$proxy_env\"; elif [ " + strictValue + " -eq 1 ]; then message=$(cat \"$proxy_error\" 2>/dev/null || true); echo \"sshx: proxy unavailable${message:+: $message}\" >&2; exit 1; fi",
	}, "; ")
}

func writeIntegrationProxyState(ctx context.Context, transport sshServerTransport, contextHome, sessionID string, env proxyEnvironment, proxyErr error) error {
	envPath := integrationProxyEnvPath(contextHome, sessionID)
	errorPath := integrationProxyErrorPath(contextHome, sessionID)
	content := env.script()
	targetPath := envPath
	otherPath := errorPath
	if proxyErr != nil {
		targetPath = errorPath
		otherPath = envPath
		content = proxyErr.Error()
	}
	script := strings.Join([]string{
		"set -eu",
		"path=\"" + strings.ReplaceAll(targetPath, `"`, `\"`) + "\"",
		"other=\"" + strings.ReplaceAll(otherPath, `"`, `\"`) + "\"",
		"mkdir -p \"${path%/*}\"",
		"tmp=\"$path.$$.tmp\"",
		"printf '%s\\n' " + shellQuote(content) + " > \"$tmp\"",
		"chmod 600 \"$tmp\"",
		"mv -f \"$tmp\" \"$path\"",
		"rm -f \"$other\"",
	}, "\n")
	return transport.ExecScript(ctx, script)
}

func removeIntegrationProxyState(transport sshServerTransport, contextHome, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	envPath := strings.ReplaceAll(integrationProxyEnvPath(contextHome, sessionID), `"`, `\"`)
	errorPath := strings.ReplaceAll(integrationProxyErrorPath(contextHome, sessionID), `"`, `\"`)
	_ = transport.ExecScript(ctx, "rm -f \""+envPath+"\" \""+errorPath+"\"")
}
