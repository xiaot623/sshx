package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/sshcompat"
)

func commandTimeout(parsed *sshcompat.Parsed) (time.Duration, error) {
	argv := parsed.RemoteCommand
	if len(argv) == 0 {
		return 0, nil
	}

	var value string
	consumed := 0
	switch {
	case strings.HasPrefix(argv[0], "--timeout="):
		value = strings.TrimPrefix(argv[0], "--timeout=")
		consumed = 1
	case argv[0] == "--timeout":
		if len(argv) < 2 {
			return 0, errors.New("--timeout requires a value")
		}
		value = argv[1]
		consumed = 2
	default:
		return 0, nil
	}

	timeout, err := parseCommandTimeout(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --timeout value %q: %w", value, err)
	}
	remaining := append([]string(nil), argv[consumed:]...)
	if len(remaining) == 0 {
		return 0, errors.New("command is required after --timeout")
	}
	parsed.RemoteCommand = remaining
	parsed.Args = append(append([]string(nil), parsed.Args[:parsed.TargetIndex+1]...), remaining...)
	return timeout, nil
}

func parseCommandTimeout(value string) (time.Duration, error) {
	if value == "" {
		return 0, errors.New("value is empty")
	}
	if seconds, err := strconv.ParseUint(value, 10, 63); err == nil {
		if seconds == 0 || seconds > uint64((1<<63-1)/int64(time.Second)) {
			return 0, errors.New("duration must be greater than zero and fit in a duration")
		}
		return time.Duration(seconds) * time.Second, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, errors.New("use seconds or a duration such as 500ms, 30s, or 2m")
	}
	if timeout <= 0 {
		return 0, errors.New("duration must be greater than zero")
	}
	return timeout, nil
}

func withCommandTimeout(ctx context.Context, timeout time.Duration) (context.Context, func()) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
