package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const Version = "0.1"

func main() {
	version := flag.Bool("version", false, "Print version and exit")
	maxRetries := flag.Int("max-retries", 5, "Maximum number of rebase/retry attempts")
	flag.Parse()

	if *version {
		fmt.Fprintf(os.Stderr, "merge-on-green version %s\n", Version)
		return
	}

	ctx := context.Background()
	if err := run(ctx, *maxRetries); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, maxRetries int) error {
	ciCmd, err := detectCI(ctx)
	if err != nil {
		return err
	}
	slog.Info("detected CI", "tool", ciCmd)

	branch, err := currentBranch(ctx)
	if err != nil {
		return err
	}
	slog.Info("current branch", "branch", branch)

	defaultBranch, err := defaultRemoteBranch(ctx)
	if err != nil {
		return err
	}
	slog.Info("default remote branch", "branch", defaultBranch)

	if branch == defaultBranch {
		return fmt.Errorf("already on default branch %q, nothing to merge", defaultBranch)
	}

	for attempt := range maxRetries {
		if attempt > 0 {
			slog.Info("retrying", "attempt", attempt+1)
		}

		if err := gitFetch(ctx); err != nil {
			return fmt.Errorf("fetching origin: %w", err)
		}

		rebasing, err := needsRebase(ctx, defaultBranch)
		if err != nil {
			return err
		}
		if rebasing {
			slog.Info("rebasing onto default branch", "base", defaultBranch)
			if err := rebase(ctx, defaultBranch); err != nil {
				return fmt.Errorf("rebase onto origin/%s failed: %w", defaultBranch, err)
			}
			slog.Info("force pushing rebased branch")
			if err := forcePush(ctx, branch); err != nil {
				return fmt.Errorf("force push failed: %w", err)
			}
		}

		slog.Info("waiting for CI to complete")
		if err := waitForCI(ctx, ciCmd); err != nil {
			// CI failure: the wait command already printed the output.
			return fmt.Errorf("CI failed on branch %s", branch)
		}
		slog.Info("CI passed")

		// Fetch again in case the default branch moved while CI was running.
		if err := gitFetch(ctx); err != nil {
			return fmt.Errorf("fetching origin: %w", err)
		}

		rebasing, err = needsRebase(ctx, defaultBranch)
		if err != nil {
			return err
		}
		if rebasing {
			slog.Info("default branch moved during CI, need to rebase again")
			continue
		}

		slog.Info("pushing to default branch", "branch", defaultBranch)
		if err := pushToDefault(ctx, defaultBranch); err != nil {
			slog.Warn("push failed, will retry", "error", err.Error())
			continue
		}

		slog.Info("merged", "branch", branch, "into", defaultBranch)
		return nil
	}

	return fmt.Errorf("exceeded maximum retry attempts (%d)", maxRetries)
}

// detectCI returns "github-actions" or "buildkite" based on which CI
// configuration directory exists in the git root.
func detectCI(ctx context.Context) (string, error) {
	root, err := gitRoot(ctx)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(filepath.Join(root, ".github", "workflows")); err == nil && info.IsDir() {
		return "github-actions", nil
	}
	if info, err := os.Stat(filepath.Join(root, ".buildkite")); err == nil && info.IsDir() {
		return "buildkite", nil
	}
	return "", fmt.Errorf("no .github/workflows or .buildkite directory in %s", root)
}

func gitRoot(ctx context.Context) (string, error) {
	out, err := execOutput(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("getting git root: %w", err)
	}
	return out, nil
}

func currentBranch(ctx context.Context) (string, error) {
	out, err := execOutput(ctx, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("getting current branch (detached HEAD?): %w", err)
	}
	return out, nil
}

// defaultRemoteBranch returns the name of the default branch on origin
// (e.g. "main" or "master").
func defaultRemoteBranch(ctx context.Context) (string, error) {
	out, err := execOutput(ctx, "git", "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err == nil {
		return strings.TrimPrefix(out, "origin/"), nil
	}
	for _, name := range []string{"main", "master"} {
		if execSilent(ctx, "git", "rev-parse", "--verify", "origin/"+name) == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not determine default remote branch; try: git remote set-head origin --auto")
}

func gitFetch(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "origin")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// needsRebase reports whether origin/<defaultBranch> is NOT an ancestor of
// HEAD, meaning a rebase is required before a fast-forward push.
func needsRebase(ctx context.Context, defaultBranch string) (bool, error) {
	err := execSilent(ctx, "git", "merge-base", "--is-ancestor", "origin/"+defaultBranch, "HEAD")
	if err == nil {
		return false, nil
	}
	// Distinguish "not ancestor" (exit 1) from other errors.
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("checking merge-base: %w", err)
}

func rebase(ctx context.Context, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "rebase", "origin/"+defaultBranch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = execSilent(ctx, "git", "rebase", "--abort")
		return err
	}
	return nil
}

func forcePush(ctx context.Context, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "--force-with-lease", "origin", branch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForCI(ctx context.Context, ciCmd string) error {
	// TODO: add --quiet to buildkite, then uncomment here.
	cmd := exec.CommandContext(ctx, ciCmd, "wait" /*, "--quiet" */)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func pushToDefault(ctx context.Context, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "origin", "HEAD:refs/heads/"+defaultBranch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func execOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func execSilent(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
