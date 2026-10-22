package cfprobe

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const commandOutputTimeout = 30 * time.Second

func commandOutput(name string, args ...string) string {
	out, _ := commandOutputChecked(name, args...)
	return out
}

func commandOutputChecked(name string, args ...string) (string, error) {
	return commandOutputWithTimeout(commandOutputTimeout, name, args...)
}

func commandOutputWithTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Bound the wait for inherited stdout/stderr pipes as well as the process.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s timed out: %w", name, ctx.Err())
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
