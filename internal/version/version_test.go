package version

import "testing"

func TestStringIncludesBuildFields(t *testing.T) {
	originalVersion, originalCommit, originalBuiltAt := Version, Commit, BuiltAt
	t.Cleanup(func() {
		Version, Commit, BuiltAt = originalVersion, originalCommit, originalBuiltAt
	})

	Version = "v1.2.3"
	Commit = "abc123"
	BuiltAt = "2026-07-23T00:00:00Z"

	const want = "v1.2.3 commit=abc123 built_at=2026-07-23T00:00:00Z"
	if got := String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
