# Changelog

This file summarizes the notable changes in each release. `merge-on-green`
did not previously have a changelog; since the project is under a year old,
this covers its entire history.

## Unreleased

- Serialize merges across worktrees of the same local repository before
  rebasing or pushing. Concurrent callers wait without starting CI that
  another merge would invalidate. The lock releases automatically on exit
  or a crash; Ctrl-C and SIGTERM cancel waiting. This is local serialization,
  not a FIFO or cross-host queue; independent clones and direct pushes still
  require the existing remote-movement checks.
- Bound the entire invocation, including queueing and holding the merge lock,
  with `--timeout` (default 30 minutes; must be positive). On timeout or
  interruption, stop local subprocess groups before releasing the lock. A
  queued timeout never steals another caller's lock; no lock-file cleanup is
  necessary. Locking and signal operations use `golang.org/x/sys/unix`.
- Require Go 1.26, matching the oldest CI toolchain and the x/sys dependency.
- Add `--remote` (default `origin`) to select the Git remote for commit checks,
  CI waits, branch lookup, rebases, pushes, and post-merge cleanup. For example,
  `merge-on-green --remote upstream --branch feature`.

## 0.4 - 2026-09-11

- Set `GIT_SEQUENCE_EDITOR=true` and `GIT_EDITOR=true` on the rebase
  subprocess so a stray interactive editor prompt can never hang an
  unattended run.
- Test against Go 1.26.x and 1.27.x in CI (dropped 1.25.x).

## 0.3 - 2026-09-09

- Retry a CI wait that could not reach a verdict, and return an error once
  retries are exhausted, instead of treating an inconclusive wait as
  success.
- Report a missing CI command (e.g. `gh` or `buildkite-agent`) with a clear
  error instead of failing opaquely.
- Check code formatting and generated files only on the newest Go version in
  the CI matrix, since older toolchains can't install current tooling.
- Require GitHub Actions workflow files, not just a `.github` directory, to
  detect GitHub Actions as the CI backend.
- Cancel an in-progress CI wait early if the default branch moves
  underneath it, so the tool can rebase and retry instead of waiting on
  stale CI.
- After merging, refresh the primary checkout to the new default-branch tip
  and remove the merged branch's worktree and local branch.
- Add `--skip-ci` for repositories with no CI configured.
- Add `--verbose` to show CI wait output directly instead of running it
  quietly.
- Add `--branch` to operate on a branch other than the current one,
  creating a temporary worktree for it if needed.
- Push the local branch to origin, and require it to be pushed, before
  waiting for CI.
- Add a GitHub Actions CI workflow and daily Dependabot updates, later given
  a seven-day cooldown.
- Initial release: rebase the current branch onto the default branch,
  force-push it, wait for CI (GitHub Actions or Buildkite), and merge into
  the default branch with a fast-forward push once CI passes.
