package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/locald"
)

func (r *Runner) defaultStartBridge(ctx context.Context, target string, sshArgs []string, remoteHome string) (func(), error) {
	token, err := r.fetchRemoteToken(ctx, sshArgs, remoteHome)
	if err != nil {
		return nil, err
	}
	localDaemonSocket := defaultLocalDaemonSocketPath()
	if r.forwardPorts {
		if err := r.ensureLocalDaemon(ctx, localDaemonSocket); err != nil {
			return nil, err
		}
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(
		bridgeCtx,
		r.SSHPath,
		sshCommandArgs(sshArgs, remoteShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" socket-proxy --socket \"$SSHX_SERVER_HOME/sock\""))...,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = r.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	conn := bridge.NewReadWriteCloser(stdout, stdin, func() error {
		_ = stdin.Close()
		cancel()
		return nil
	})
	readyCh := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		opts := bridge.ClientOptions{
			Ready: readyCh,
			Allow: func(argv []string) bool {
				return r.commandBridge && r.commandPolicy.Allows(argv)
			},
		}
		if r.forwardPorts {
			opts.OnPortObserved = func(port int) {
				_, err := locald.ClientRequest(bridgeCtx, localDaemonSocket, locald.Request{
					Type:           locald.TypeEnsurePort,
					SSHPath:        r.SSHPath,
					Target:         target,
					SSHArgs:        append([]string(nil), sshArgs...),
					RemotePort:     port,
					DomainsEnabled: r.domains.Enabled,
					DomainSuffix:   domainSuffix(r.domains),
					DNSAddr:        domainDNSAddr(r.domains),
				})
				if err != nil {
					fmt.Fprintf(r.Stderr, "sshx: forward remote port %d: %v\n", port, err)
				}
			}
		}
		errCh <- bridge.RunClientConnWithOptions(bridgeCtx, conn, opts, token)
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			cancel()
			return nil, err
		}
	case err := <-errCh:
		cancel()
		return nil, err
	case <-time.After(2 * time.Second):
		cancel()
		return nil, errors.New("timed out waiting for command bridge handshake")
	}
	stop := func() {
		cancel()
		_ = stdin.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
		}
	}
	return stop, nil
}

func (r *Runner) fetchRemoteToken(ctx context.Context, sshArgs []string, remoteHome string) (string, error) {
	cmd := exec.CommandContext(ctx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(remoteServerEnvScript(remoteHome)+"; cat \"$SSHX_SERVER_HOME/server-info\""))...)
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var info bridge.ServerInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return "", err
	}
	return info.Token, nil
}

func (r *Runner) ensureLocalDaemon(ctx context.Context, socketPath string) error {
	if _, err := locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypePing}); err == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(socketPath), "local-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(context.Background(), exe, "local-daemon", "--socket", socketPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypePing}); err == nil {
			_ = logFile.Close()
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = logFile.Close()
	if lastErr != nil {
		return lastErr
	}
	return errors.New("local daemon did not start")
}
