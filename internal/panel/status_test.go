package panel

import (
	"testing"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

func TestAgentStatusLabelValueMatchesTypedStatusLabels(t *testing.T) {
	for _, test := range []struct {
		status string
		want   string
	}{
		{status: "done", want: "done ✅"},
		{status: "working", want: "working ⏳"},
		{status: "blocked", want: "blocked ⁉️"},
		{status: "idle", want: "idle 💤"},
		{status: "unknown", want: "unknown ❔"},
		{status: "future", want: "future"},
	} {
		if got := AgentStatusLabelValue(test.status); got != test.want {
			t.Fatalf("AgentStatusLabelValue(%q) = %q, want %q", test.status, got, test.want)
		}
		if got := AgentStatusLabel(herdr.AgentStatus(test.status)); got != test.want {
			t.Fatalf("AgentStatusLabel(%q) = %q, want %q", test.status, got, test.want)
		}
	}
}
