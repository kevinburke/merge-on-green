package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMergeTimeoutReleasesLock(t *testing.T) {
	root := t.TempDir()
	gitTestCommand(t, root, "init", "--initial-branch=main")
	gitTestCommand(t, root, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	mkdirAll(t, bin)
	// Hang immediately after the lock is acquired, while selecting the branch.
	script := `#!/bin/sh
set -eu
if [ "$*" = 'rev-parse --show-toplevel' ]; then
    : > "$MERGE_STARTED"
    sleep 30 &
    wait
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("MERGE_STARTED", filepath.Join(root, "started"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(root)
	started := time.Now()
	err = runWithTimeout(context.Background(), "", "origin", 1, false, true, 500*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("hung merge returned %v, want a timeout", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("timeout did not stop the hung command promptly")
	}
	if _, err := os.Stat(filepath.Join(root, "started")); err != nil {
		t.Fatalf("test did not reach the command while holding the lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := acquireMergeLock(ctx, root)
	if err != nil {
		t.Fatalf("timed-out merge left a lock behind: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMergeTimeoutWhileQueuedDoesNotStealLock(t *testing.T) {
	root := t.TempDir()
	gitTestCommand(t, root, "init", "--initial-branch=main")
	t.Chdir(root)
	lock, err := acquireMergeLock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err = runWithTimeout(context.Background(), "", "origin", 1, false, true, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued merge returned %v, want a timeout", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if next, err := acquireMergeLock(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		if next != nil {
			next.Close()
		}
		t.Fatalf("waiter stole the holder's lock: %v", err)
	}
}

func TestMergeTimeoutMustBePositive(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		if err := runWithTimeout(context.Background(), "", "origin", 1, false, true, timeout); err == nil || !strings.Contains(err.Error(), "positive") {
			t.Fatalf("timeout %v: got %v", timeout, err)
		}
	}
}

func TestCommandCancellationStopsDescendants(t *testing.T) {
	if root := os.Getenv("MERGE_DESCENDANT_ROOT"); root != "" {
		lock, err := acquireMergeLock(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		return
	}
	// A grandchild holds its own lock and inherits the output pipe. Merely
	// killing the direct shell or closing its pipes must not pass this test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	gitTestCommand(t, root, "init", "--initial-branch=main")
	ready := filepath.Join(root, "ready")
	cmd := commandContext(ctx, "sh", "-c", `trap '' TERM; "$1" -test.run=^TestCommandCancellationStopsDescendants$ & wait`, "test", os.Args[0])
	cmd.Env = append(os.Environ(), "MERGE_DESCENDANT_ROOT="+root)
	done := make(chan error, 1)
	go func() {
		_, err := cmd.CombinedOutput()
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled command reported success")
		}
	case <-time.After(5 * time.Second):
		// Clean up the fixture even if the cancellation implementation regresses.
		unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		<-done
		t.Fatal("command or descendant kept running after cancellation")
	}
	lockCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	lock, err := acquireMergeLock(lockCtx, root)
	if err != nil {
		unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		t.Fatalf("descendant survived cancellation and still holds its lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
