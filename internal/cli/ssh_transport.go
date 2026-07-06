package cli

import (
	"context"
	"io"
)

type sshServerTransport struct {
	r       *Runner
	sshArgs []string
}

func (t sshServerTransport) OutputScript(ctx context.Context, script string) ([]byte, error) {
	return t.r.ExecOutput(ctx, t.r.SSHPath, internalSSHArgs(t.sshArgs, remoteShell(script)))
}

func (t sshServerTransport) ExecScript(ctx context.Context, script string) error {
	return t.r.Exec(ctx, t.r.SSHPath, internalSSHArgs(t.sshArgs, remoteShell(script)))
}

func (t sshServerTransport) ExecInputScript(ctx context.Context, script string, stdin io.Reader) error {
	return t.r.ExecInput(ctx, t.r.SSHPath, sshCommandArgs(t.sshArgs, remoteShell(script)), stdin)
}
