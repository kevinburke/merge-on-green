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

type worktreeInfo struct {
	path   string
	branch string
}

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

		if err := ensureBranchPushed(ctx, branch); err != nil {
			return err
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

		if err := postMergeCleanup(ctx, branch, defaultBranch); err != nil {
			slog.Warn("pushed successfully but post-merge cleanup failed", "branch", branch, "default_branch", defaultBranch, "error", err.Error())
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

func ensureBranchPushed(ctx context.Context, branch string) error {
	headSHA, err := execOutput(ctx, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}

	remoteRefs, err := execOutput(ctx, "git", "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin", "--contains", "HEAD")
	if err != nil {
		return fmt.Errorf("checking whether %s is on origin: %w", headSHA, err)
	}
	if remoteRefs == "" {
		return fmt.Errorf("current commit %s is not present on origin; run git push origin %s before waiting for CI", headSHA, branch)
	}
	return nil
}

func waitForCI(ctx context.Context, ciCmd string) error {
	// TODO: add --quiet to buildkite, then uncomment here.
	cmd := exec.CommandContext(ctx, ciCmd, "wait" /*, "--quiet" */)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func postMergeCleanup(ctx context.Context, branch, defaultBranch string) error {
	worktrees, err := listWorktrees(ctx)
	if err != nil {
		return err
	}

	controlDir, err := gitRoot(ctx)
	if err != nil {
		return err
	}
	defaultWorktree, hasDefaultWorktree := findOptionalWorktreeForBranch(worktrees, defaultBranch)
	branchWorktree, hasBranchWorktree := findOptionalWorktreeForBranch(worktrees, branch)

	if hasDefaultWorktree {
		controlDir = defaultWorktree.path
		err = resetDefaultBranchWorktree(ctx, defaultWorktree.path, defaultBranch)
	} else {
		err = updateDefaultBranchRef(ctx, controlDir, defaultBranch)
	}
	if err != nil {
		return err
	}

	if hasBranchWorktree {
		if hasDefaultWorktree && branchWorktree.path == defaultWorktree.path {
			return fmt.Errorf("refusing to remove worktree %s because branch %s is also the default branch worktree", branchWorktree.path, branch)
		}
		if isCurrentWorktree(branchWorktree.path, controlDir) {
			if hasDefaultWorktree {
				return fmt.Errorf("refusing to delete branch %s because merge-on-green is running from the checkout at %s", branch, branchWorktree.path)
			}
			if err := switchCurrentCheckoutToDefault(ctx, controlDir, branch, defaultBranch); err != nil {
				return err
			}
			slog.Info("deleting merged local branch", "branch", branch)
			if err := runGitCommand(ctx, controlDir, "branch", "-d", branch); err != nil {
				return fmt.Errorf("deleting local branch %s: %w", branch, err)
			}
			return nil
		}
		if err := ensureCleanWorktree(ctx, branchWorktree.path, branch); err != nil {
			return err
		}
		slog.Info("removing merged branch worktree", "branch", branch, "path", branchWorktree.path)
		if err := runGitCommand(ctx, controlDir, "worktree", "remove", branchWorktree.path); err != nil {
			return fmt.Errorf("removing worktree for branch %s: %w", branch, err)
		}
	}

	slog.Info("deleting merged local branch", "branch", branch)
	if err := runGitCommand(ctx, controlDir, "branch", "-d", branch); err != nil {
		return fmt.Errorf("deleting local branch %s: %w", branch, err)
	}
	return nil
}

func listWorktrees(ctx context.Context) ([]worktreeInfo, error) {
	out, err := execOutput(ctx, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %w", err)
	}

	blocks := strings.Split(strings.TrimSpace(out), "\n\n")
	worktrees := make([]worktreeInfo, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var wt worktreeInfo
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				wt.path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch refs/heads/"):
				wt.branch = strings.TrimPrefix(line, "branch refs/heads/")
			}
		}
		if wt.path != "" {
			worktrees = append(worktrees, wt)
		}
	}
	if len(worktrees) == 0 {
		return nil, fmt.Errorf("no git worktrees found")
	}
	return worktrees, nil
}

func findOptionalWorktreeForBranch(worktrees []worktreeInfo, branch string) (worktreeInfo, bool) {
	for _, wt := range worktrees {
		if wt.branch == branch {
			return wt, true
		}
	}
	return worktreeInfo{}, false
}

func resetDefaultBranchWorktree(ctx context.Context, worktreePath, defaultBranch string) error {
	slog.Info("updating default branch worktree", "branch", defaultBranch, "path", worktreePath)
	if err := runGitCommand(ctx, worktreePath, "fetch", "origin"); err != nil {
		return fmt.Errorf("fetching origin in %s: %w", worktreePath, err)
	}
	if err := runGitCommand(ctx, worktreePath, "reset", "--hard", "origin/"+defaultBranch); err != nil {
		return fmt.Errorf("resetting %s to origin/%s in %s: %w", defaultBranch, defaultBranch, worktreePath, err)
	}
	return nil
}

func updateDefaultBranchRef(ctx context.Context, controlDir, defaultBranch string) error {
	slog.Info("updating local default branch ref", "branch", defaultBranch)
	if err := runGitCommand(ctx, controlDir, "fetch", "origin"); err != nil {
		return fmt.Errorf("fetching origin in %s: %w", controlDir, err)
	}
	if err := runGitCommand(ctx, controlDir, "update-ref", "refs/heads/"+defaultBranch, "refs/remotes/origin/"+defaultBranch); err != nil {
		return fmt.Errorf("updating local branch ref %s in %s: %w", defaultBranch, controlDir, err)
	}
	return nil
}

func switchCurrentCheckoutToDefault(ctx context.Context, controlDir, branch, defaultBranch string) error {
	if err := ensureCleanWorktree(ctx, controlDir, branch); err != nil {
		return err
	}
	slog.Info("switching current checkout to default branch", "from", branch, "to", defaultBranch)
	if err := runGitCommand(ctx, controlDir, "checkout", defaultBranch); err != nil {
		return fmt.Errorf("checking out %s in %s: %w", defaultBranch, controlDir, err)
	}
	if err := runGitCommand(ctx, controlDir, "reset", "--hard", "origin/"+defaultBranch); err != nil {
		return fmt.Errorf("resetting %s to origin/%s in %s: %w", defaultBranch, defaultBranch, controlDir, err)
	}
	return nil
}

func ensureCleanWorktree(ctx context.Context, worktreePath, branch string) error {
	if err := execSilentInDir(ctx, worktreePath, "git", "diff", "--quiet", "HEAD"); err != nil {
		return fmt.Errorf("refusing to remove worktree %s for branch %s: uncommitted changes present", worktreePath, branch)
	}
	if err := execSilentInDir(ctx, worktreePath, "git", "diff", "--cached", "--quiet", "HEAD"); err != nil {
		return fmt.Errorf("refusing to remove worktree %s for branch %s: staged changes present", worktreePath, branch)
	}
	out, err := execOutputInDir(ctx, worktreePath, "git", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("checking for untracked files in %s: %w", worktreePath, err)
	}
	if out != "" {
		return fmt.Errorf("refusing to remove worktree %s for branch %s: untracked files present", worktreePath, branch)
	}
	return nil
}

func isCurrentWorktree(targetPath, currentPath string) bool {
	targetClean := filepath.Clean(targetPath)
	currentClean := filepath.Clean(currentPath)
	return currentClean == targetClean || strings.HasPrefix(currentClean, targetClean+string(os.PathSeparator))
}

func pushToDefault(ctx context.Context, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "origin", "HEAD:refs/heads/"+defaultBranch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func execOutput(ctx context.Context, name string, args ...string) (string, error) {
	return execOutputInDir(ctx, "", name, args...)
}

func execOutputInDir(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func execSilent(ctx context.Context, name string, args ...string) error {
	return execSilentInDir(ctx, "", name, args...)
}

func execSilentInDir(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func runGitCommand(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
