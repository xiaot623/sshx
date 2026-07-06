package cli

import (
	"context"
	"io"
)

type dockerServerTransport struct {
	r         *Runner
	container string
}

func (t dockerServerTransport) OutputScript(ctx context.Context, script string) ([]byte, error) {
	return t.r.ExecOutput(ctx, t.r.DockerPath, dockerInternalExecArgs(t.container, dockerShell(script)...))
}

func (t dockerServerTransport) ExecScript(ctx context.Context, script string) error {
	return t.r.Exec(ctx, t.r.DockerPath, dockerInternalExecArgs(t.container, dockerShell(script)...))
}

func (t dockerServerTransport) ExecInputScript(ctx context.Context, script string, stdin io.Reader) error {
	return t.r.ExecInput(ctx, t.r.DockerPath, dockerExecInputArgs(t.container, dockerShell(script)...), stdin)
}
