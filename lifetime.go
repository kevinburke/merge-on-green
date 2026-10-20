package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultTimeout = 30 * time.Minute

// Bound queueing and the entire lock-holding operation together. Never steal
// an expired lock: stop its owner first so two merges cannot overlap.
func runWithTimeout(ctx context.Context, branch, remote string, maxRetries int, verbose, skipCI bool, timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := run(ctx, branch, remote, maxRetries, verbose, skipCI)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("merge stopped (timeout %s, including queue and CI); this invocation holds no merge lock: %w", timeout, errors.Join(ctx.Err(), err))
	}
	return err
}

// commandContext cancels the entire local command group, including SSH, Git
// helpers and CI descendants. Killing only the direct child can leave a helper
// running or holding an output pipe open and prevent release of the merge lock.
func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// os/exec requires this standard-library type. All system calls below use
	// x/sys/unix; this is the only direct reference to the syscall package.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL); errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	// Also bound output draining if a descendant escaped the process group.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
