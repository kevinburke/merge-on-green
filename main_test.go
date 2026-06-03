package main

import (
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
