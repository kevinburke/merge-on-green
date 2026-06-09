package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDetectCIInRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, root string)
		want    string
		wantErr string
	}{
		{
			name: "github actions requires workflow content",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".github", "workflows"))
			},
			wantErr: "no .yml/.yaml files in .github/workflows",
		},
		{
			name: "empty github actions workflows falls back to buildkite",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".github", "workflows"))
				mkdirAll(t, filepath.Join(root, ".buildkite"))
			},
			want: "buildkite",
		},
		{
			name: "github actions workflow content wins",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"), []byte("name: CI\n"))
				mkdirAll(t, filepath.Join(root, ".buildkite"))
			},
			want: "github-actions",
		},
		{
			name: "workflow subdirectories do not count as workflow content",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".github", "workflows", "nested"))
			},
			wantErr: "no .yml/.yaml files in .github/workflows",
		},
		{
			name: "workflow placeholders do not count as workflow content",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, ".github", "workflows", ".gitkeep"), nil)
			},
			wantErr: "no .yml/.yaml files in .github/workflows",
		},
		{
			name: "buildkite directory still detects buildkite",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".buildkite"))
			},
			want: "buildkite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			tt.setup(t, root)

			got, err := detectCIInRoot(root)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("detectCIInRoot(%q) succeeded, want error containing %q", root, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("detectCIInRoot(%q) error = %q, want substring %q", root, err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("detectCIInRoot(%q) error = %v", root, err)
			}
			if got != tt.want {
				t.Fatalf("detectCIInRoot(%q) = %q, want %q", root, got, tt.want)
			}
		})
	}
}

func TestWaitCommandArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ciCmd   string
		verbose bool
		want    []string
	}{
		{
			name:  "github actions cancels older runs quietly by default",
			ciCmd: "github-actions",
			want:  []string{"wait", "--cancel-previous-runs", "--quiet"},
		},
		{
			name:  "buildkite waits quietly by default",
			ciCmd: "buildkite",
			want:  []string{"wait", "--quiet"},
		},
		{
			name:    "github actions verbose disables quiet mode",
			ciCmd:   "github-actions",
			verbose: true,
			want:    []string{"wait", "--cancel-previous-runs"},
		},
		{
			name:    "buildkite verbose disables quiet mode",
			ciCmd:   "buildkite",
			verbose: true,
			want:    []string{"wait"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := waitCommandArgs(tt.ciCmd, tt.verbose)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("waitCommandArgs(%q, %t) = %v, want %v", tt.ciCmd, tt.verbose, got, tt.want)
			}
		})
	}
}

func TestPostMergeCleanupSwitchesPrimaryCheckoutToDefault(t *testing.T) {
	tmp := t.TempDir()
	primary := filepath.Join(tmp, "repo")
	mkdirAll(t, filepath.Join(primary, ".git"))

	binDir := filepath.Join(tmp, "bin")
	mkdirAll(t, binDir)
	logPath := filepath.Join(tmp, "git.log")
	scriptPath := filepath.Join(binDir, "git")
	script := `#!/bin/sh
set -eu
printf '%s|%s\n' "$PWD" "$*" >> "$GIT_LOG"

case "$*" in
"worktree list --porcelain")
	printf 'worktree %s\nbranch refs/heads/feature\n' "$PRIMARY_CHECKOUT"
	;;
"rev-parse --show-toplevel")
	printf '%s\n' "$PRIMARY_CHECKOUT"
	;;
"fetch origin"|"update-ref refs/heads/main refs/remotes/origin/main"|"diff --quiet HEAD"|"diff --cached --quiet HEAD"|"ls-files --others --exclude-standard"|"checkout main"|"reset --hard origin/main"|"branch -d feature")
	;;
*)
	echo "unexpected git command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q): %v", scriptPath, err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_LOG", logPath)
	t.Setenv("PRIMARY_CHECKOUT", primary)

	if err := postMergeCleanup(context.Background(), "feature", "main"); err != nil {
		t.Fatalf("postMergeCleanup: %v", err)
	}

	logContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", logPath, err)
	}
	log := string(logContent)
	if strings.Contains(log, "worktree remove") {
		t.Fatalf("postMergeCleanup tried to remove the primary checkout:\n%s", log)
	}
	if want := primary + "|checkout main"; !strings.Contains(log, want) {
		t.Fatalf("postMergeCleanup did not check out main in the primary checkout; want log containing %q, got:\n%s", want, log)
	}
	if want := primary + "|branch -d feature"; !strings.Contains(log, want) {
		t.Fatalf("postMergeCleanup did not delete feature from the primary checkout; want log containing %q, got:\n%s", want, log)
	}
}

func TestPostMergeCleanupLeavesCurrentLinkedWorktreeOnBranch(t *testing.T) {
	tmp := t.TempDir()
	defaultWorktree := filepath.Join(tmp, "repo")
	branchWorktree := filepath.Join(tmp, "repo", "worktrees", "feature")
	mkdirAll(t, filepath.Join(defaultWorktree, ".git"))
	mkdirAll(t, branchWorktree)
	writeFile(t, filepath.Join(branchWorktree, ".git"), []byte("gitdir: ../../.git/worktrees/feature\n"))

	binDir := filepath.Join(tmp, "bin")
	mkdirAll(t, binDir)
	logPath := filepath.Join(tmp, "git.log")
	scriptPath := filepath.Join(binDir, "git")
	script := `#!/bin/sh
set -eu
printf '%s|%s\n' "$PWD" "$*" >> "$GIT_LOG"

case "$*" in
"worktree list --porcelain")
	printf 'worktree %s\nbranch refs/heads/main\n\nworktree %s\nbranch refs/heads/feature\n' "$DEFAULT_WORKTREE" "$BRANCH_WORKTREE"
	;;
"rev-parse --show-toplevel")
	printf '%s\n' "$BRANCH_WORKTREE"
	;;
"fetch origin"|"reset --hard origin/main")
	;;
*)
	echo "unexpected git command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q): %v", scriptPath, err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_LOG", logPath)
	t.Setenv("DEFAULT_WORKTREE", defaultWorktree)
	t.Setenv("BRANCH_WORKTREE", branchWorktree)

	if err := postMergeCleanup(context.Background(), "feature", "main"); err != nil {
		t.Fatalf("postMergeCleanup: %v", err)
	}

	logContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", logPath, err)
	}
	log := string(logContent)
	for _, unexpected := range []string{"checkout main", "worktree remove", "branch -d feature"} {
		if strings.Contains(log, unexpected) {
			t.Fatalf("postMergeCleanup ran %q for current linked worktree:\n%s", unexpected, log)
		}
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
