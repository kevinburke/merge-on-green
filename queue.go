package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// acquireMergeLock serializes the entire rebase/push/CI/merge cycle across
// worktrees of this local repository. Locking just the final push is too late:
// every contender would already have built a commit the winner invalidates.
// Separate clones and hosts do not share this lock, so the remote movement
// checks and fast-forward-only push remain necessary.
func acquireMergeLock(ctx context.Context, dir string) (*os.File, error) {
	commonDir, err := execOutputInDir(ctx, dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("finding merge lock directory: %w", err)
	}
	path := filepath.Join(commonDir, "merge-on-green.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening merge lock: %w", err)
	}
	// Never unlink this file: a waiter may already have the inode open.
	// Close releases the kernel lock, including on a crash or SIGKILL. Go
	// opens the descriptor close-on-exec, so Git and CI children cannot keep it.
	waiting := false
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("waiting for merge turn: %w", err)
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			slog.Info("acquired merge turn", "lock", path)
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, fmt.Errorf("locking merge turn: %w", err)
		}
		if !waiting {
			slog.Info("waiting for another merge to finish before rebasing or pushing", "lock", path)
			waiting = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
}
