package main

import (
	"bytes"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/version"
)

func TestRun(t *testing.T) {
	originalVersion, originalCommit, originalBuiltAt := version.Version, version.Commit, version.BuiltAt
	t.Cleanup(func() {
		version.Version, version.Commit, version.BuiltAt = originalVersion, originalCommit, originalBuiltAt
	})

	version.Version = "v1.2.3"
	version.Commit = "abc123"
	version.BuiltAt = "2026-07-23T00:00:00Z"

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "显示版本",
			args:       []string{"--version"},
			wantStdout: version.String() + "\n",
		},
		{
			name:       "没有参数",
			wantCode:   2,
			wantStderr: "用法: herdr-pal --version\n",
		},
		{
			name:       "未知参数",
			args:       []string{"--unknown"},
			wantCode:   2,
			wantStderr: "用法: herdr-pal --version\n",
		},
		{
			name:       "额外参数",
			args:       []string{"--version", "extra"},
			wantCode:   2,
			wantStderr: "用法: herdr-pal --version\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if got := run(tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Errorf("run() = %d, want %d", got, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			if got := stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}
