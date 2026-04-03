package main

import (
	"reflect"
	"testing"
)

func TestWaitCommandArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ciCmd string
		want  []string
	}{
		{
			name:  "github actions cancels older runs",
			ciCmd: "github-actions",
			want:  []string{"wait", "--cancel-previous-runs"},
		},
		{
			name:  "buildkite keeps default wait args",
			ciCmd: "buildkite",
			want:  []string{"wait"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := waitCommandArgs(tt.ciCmd)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("waitCommandArgs(%q) = %v, want %v", tt.ciCmd, got, tt.want)
			}
		})
	}
}
