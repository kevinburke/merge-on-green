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
	"time"
)

type worktreeInfo struct {
	path   string
	branch string
}

func main() {
	branch := flag.String("branch", "", "Branch to merge; defaults to the current branch")
	version := flag.Bool("version", false, "Print version and exit")
	maxRetries := flag.Int("max-retries", 5, "Maximum number of rebase/retry attempts")
	flag.Parse()

	if *version {
		fmt.Fprintf(os.Stderr, "merge-on-green version %s\n", Version)
		return
	}

	ctx := context.Background()
	if err := run(ctx, *branch, *maxRetries); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, requestedBranch string, maxRetries int) error {
	ciCmd, err := detectCI(ctx)
	if err != nil {
		return err
	}
	slog.Info("detected CI", "tool", ciCmd)

	branch, branchDir, cleanup, err := resolveBranchDir(ctx, requestedBranch)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer func() {
			if err := cleanup(ctx); err != nil {
				slog.Warn("temporary branch cleanup failed", "branch", branch, "error", err.Error())
			}
		}()
	}
	slog.Info("selected branch", "branch", branch, "path", branchDir)

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

		if err := gitFetch(ctx, branchDir); err != nil {
			return fmt.Errorf("fetching origin: %w", err)
		}

		rebasing, err := needsRebase(ctx, branchDir, defaultBranch)
		if err != nil {
			return err
		}
		if rebasing {
			slog.Info("rebasing onto default branch", "base", defaultBranch)
			if err := rebase(ctx, branchDir, defaultBranch); err != nil {
				return fmt.Errorf("rebase onto origin/%s failed: %w", defaultBranch, err)
			}
			slog.Info("force pushing rebased branch")
			if err := forcePush(ctx, branchDir, branch); err != nil {
				return fmt.Errorf("force push failed: %w", err)
			}
		}

		if err := ensureBranchPushed(ctx, branchDir, branch); err != nil {
			return err
		}

		slog.Info("waiting for CI to complete")
		moved, ciErr := waitForCIOrBranchMove(ctx, branchDir, ciCmd, defaultBranch)
		if moved {
			continue
		}
		if ciErr != nil {
			// CI failure: the wait command already printed the output.
			return fmt.Errorf("CI failed on branch %s", branch)
		}
		slog.Info("CI passed")

		// Final check: the default branch may have moved between the
		// last poll tick and CI completion.
		if err := gitFetch(ctx, branchDir); err != nil {
			return fmt.Errorf("fetching origin: %w", err)
		}
		rebasing, err = needsRebase(ctx, branchDir, defaultBranch)
		if err != nil {
			return err
		}
		if rebasing {
			slog.Info("default branch moved during CI, need to rebase again")
			continue
		}

		slog.Info("pushing to default branch", "branch", defaultBranch)
		if err := pushToDefault(ctx, branchDir, defaultBranch); err != nil {
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

func resolveBranchDir(ctx context.Context, requestedBranch string) (string, string, func(context.Context) error, error) {
	controlDir, err := gitRoot(ctx)
	if err != nil {
		return "", "", nil, err
	}
	current, currentErr := currentBranch(ctx)
	if requestedBranch == "" {
		if currentErr != nil {
			return "", "", nil, currentErr
		}
		return current, controlDir, nil, nil
	}
	if currentErr == nil && requestedBranch == current {
		return requestedBranch, controlDir, nil, nil
	}

	worktrees, err := listWorktrees(ctx)
	if err != nil {
		return "", "", nil, err
	}
	if wt, ok := findOptionalWorktreeForBranch(worktrees, requestedBranch); ok {
		return requestedBranch, wt.path, nil, nil
	}

	return createTemporaryWorktree(ctx, controlDir, requestedBranch)
}

func createTemporaryWorktree(ctx context.Context, controlDir, branch string) (string, string, func(context.Context) error, error) {
	tempDir, err := os.MkdirTemp("", "merge-on-green-"+sanitizeBranchName(branch)+"-")
	if err != nil {
		return "", "", nil, fmt.Errorf("creating temporary worktree directory: %w", err)
	}

	createdLocalBranch := false
	switch {
	case branchExists(ctx, controlDir, "refs/heads/"+branch):
		if err := runGitCommand(ctx, controlDir, "worktree", "add", tempDir, branch); err != nil {
			return "", "", nil, removeTempDir(tempDir, fmt.Errorf("creating temporary worktree for local branch %q: %w", branch, err))
		}
	case branchExists(ctx, controlDir, "refs/remotes/origin/"+branch):
		if err := runGitCommand(ctx, controlDir, "worktree", "add", "--track", "-b", branch, tempDir, "origin/"+branch); err != nil {
			return "", "", nil, removeTempDir(tempDir, fmt.Errorf("creating temporary worktree for remote branch %q: %w", branch, err))
		}
		createdLocalBranch = true
	default:
		return "", "", nil, removeTempDir(tempDir, fmt.Errorf("branch %q does not exist locally or on origin", branch))
	}

	cleanup := func(ctx context.Context) error {
		if err := execSilent(ctx, "git", "-C", controlDir, "worktree", "remove", "--force", tempDir); err != nil {
			if !strings.Contains(err.Error(), "is not a working tree") {
				return fmt.Errorf("removing temporary worktree %s: %w", tempDir, err)
			}
		}
		if createdLocalBranch && branchExists(ctx, controlDir, "refs/heads/"+branch) {
			if err := execSilent(ctx, "git", "-C", controlDir, "branch", "-D", branch); err != nil {
				return fmt.Errorf("deleting temporary local branch %s: %w", branch, err)
			}
		}
		return nil
	}

	slog.Info("created temporary worktree", "branch", branch, "path", tempDir)
	return branch, tempDir, cleanup, nil
}

func branchExists(ctx context.Context, dir, ref string) bool {
	return execSilentInDir(ctx, dir, "git", "rev-parse", "--verify", ref) == nil
}

func removeTempDir(path string, err error) error {
	if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("%w (cleanup error: removing %s: %v)", err, path, removeErr)
	}
	return err
}

func sanitizeBranchName(branch string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-")
	return replacer.Replace(branch)
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

func gitFetch(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "origin")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// needsRebase reports whether origin/<defaultBranch> is NOT an ancestor of
// HEAD, meaning a rebase is required before a fast-forward push.
func needsRebase(ctx context.Context, dir, defaultBranch string) (bool, error) {
	err := execSilentInDir(ctx, dir, "git", "merge-base", "--is-ancestor", "origin/"+defaultBranch, "HEAD")
	if err == nil {
		return false, nil
	}
	// Distinguish "not ancestor" (exit 1) from other errors.
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("checking merge-base: %w", err)
}

func rebase(ctx context.Context, dir, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "rebase", "origin/"+defaultBranch)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = execSilentInDir(ctx, dir, "git", "rebase", "--abort")
		return err
	}
	return nil
}

func forcePush(ctx context.Context, dir, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "--force-with-lease", "origin", branch)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ensureBranchPushed(ctx context.Context, dir, branch string) error {
	headSHA, err := execOutputInDir(ctx, dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}

	remoteRefs, err := execOutputInDir(ctx, dir, "git", "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin", "--contains", "HEAD")
	if err != nil {
		return fmt.Errorf("checking whether %s is on origin: %w", headSHA, err)
	}
	if remoteRefs == "" {
		slog.Info("current commit is not present on origin; force pushing branch", "branch", branch, "commit", headSHA)
		if err := forcePush(ctx, dir, branch); err != nil {
			return fmt.Errorf("force pushing %s to origin: %w", branch, err)
		}
	}
	return nil
}

func waitForCI(ctx context.Context, dir, ciCmd string) error {
	cmd := exec.CommandContext(ctx, ciCmd, waitCommandArgs(ciCmd)...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitForCIOrBranchMove runs the CI wait while periodically checking
// whether the default branch has moved forward. If the default branch
// moves, the CI wait is cancelled immediately so we can rebase instead
// of waiting for a CI run whose result we will discard.
//
// Returns (true, nil) if the default branch moved, (false, nil) if CI
// passed, or (false, err) if CI failed.
func waitForCIOrBranchMove(ctx context.Context, dir, ciCmd, defaultBranch string) (bool, error) {
	ciCtx, ciCancel := context.WithCancel(ctx)
	defer ciCancel()

	ciDone := make(chan error, 1)
	go func() {
		ciDone <- waitForCI(ciCtx, dir, ciCmd)
	}()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-ciDone:
			return false, err
		case <-ticker.C:
			if err := execSilentInDir(ctx, dir, "git", "fetch", "origin"); err != nil {
				slog.Warn("background fetch failed during CI wait", "error", err.Error())
				continue
			}
			moved, err := needsRebase(ctx, dir, defaultBranch)
			if err != nil {
				slog.Warn("background rebase check failed during CI wait", "error", err.Error())
				continue
			}
			if moved {
				slog.Info("default branch moved during CI, cancelling wait to rebase")
				ciCancel()
				<-ciDone
				return true, nil
			}
		case <-ctx.Done():
			<-ciDone
			return false, ctx.Err()
		}
	}
}

func waitCommandArgs(ciCmd string) []string {
	args := []string{"wait"}
	if ciCmd == "github-actions" {
		args = append(args, "--cancel-previous-runs")
	}
	if ciCmd == "github-actions" || ciCmd == "buildkite" {
		args = append(args, "--quiet")
	}
	return args
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

func pushToDefault(ctx context.Context, dir, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "origin", "HEAD:refs/heads/"+defaultBranch)
	cmd.Dir = dir
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
