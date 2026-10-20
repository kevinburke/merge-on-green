package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMergeLockSharedAcrossWorktrees(t *testing.T) {
	root := t.TempDir()
	gitTestCommand(t, root, "init", "--initial-branch=main")
	gitTestCommand(t, root, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	worktree := filepath.Join(t.TempDir(), "feature")
	gitTestCommand(t, root, "worktree", "add", "-b", "feature", worktree)
	first, err := acquireMergeLock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if lock, err := acquireMergeLock(ctx, worktree); !errors.Is(err, context.DeadlineExceeded) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("second worktree acquired lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := acquireMergeLock(ctx, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMergeLockReleasedWhenHolderDies(t *testing.T) {
	if root := os.Getenv("MERGE_LOCK_HELPER_ROOT"); root != "" {
		lock, err := acquireMergeLock(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		// An exec'd child must not inherit the lock descriptor and keep the
		// lock alive after its parent dies.
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			child.Process.Kill()
			child.Wait()
		}()
		if err := os.WriteFile(filepath.Join(root, "child-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// The parent kills us: deferred cleanup must not be needed to unlock.
		time.Sleep(time.Minute)
		return
	}
	root := t.TempDir()
	gitTestCommand(t, root, "init", "--initial-branch=main")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMergeLockReleasedWhenHolderDies$")
	cmd.Env = append(os.Environ(), "MERGE_LOCK_HELPER_ROOT="+root)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("lock holder did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(filepath.Join(root, "child-pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Kill()
		child.Release()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if lock, err := acquireMergeLock(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("other process acquired lock: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed helper unexpectedly succeeded")
	}
	if err := unix.Kill(childPID, 0); err != nil {
		t.Fatalf("child exited before we could check lock inheritance: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireMergeLock(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
