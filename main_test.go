package main

import (
	"reflect"
	"testing"
)

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
