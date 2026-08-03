package installer

import (
	"strings"
	"testing"
)

func TestMergeHerdrConfigRemovesManagedSidecar(t *testing.T) {
	existing := []byte("[ui]\nmouse_capture = true\n\n" + managedSidecarBegin + "\n[[sidecar]]\ncommand = [\"herdr-pal\"]\n" + managedSidecarEnd + "\n\n[advanced]\nscrollback_limit_bytes = 1234\n")

	merged, err := mergeHerdrConfig(existing)
	if err != nil {
		t.Fatal(err)
	}
	text := string(merged)
	for _, forbidden := range []string{managedSidecarBegin, managedSidecarEnd, `command = ["herdr-pal"]`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("managed sidecar remains as %q:\n%s", forbidden, text)
		}
	}
	for _, want := range []string{"mouse_capture = true", "scrollback_limit_bytes = 1234"} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged config missing %q:\n%s", want, text)
		}
	}
	second, err := mergeHerdrConfig(merged)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(merged) {
		t.Fatalf("migration is not idempotent:\nfirst:\n%s\nsecond:\n%s", merged, second)
	}
}

func TestMergeHerdrConfigKeepsUnmanagedSidecars(t *testing.T) {
	existing := []byte("onboarding = false\n\n[[sidecar]]\ncommand = [\"herdr-pal\"]\n\n[[sidecar]]\ncommand = [\"metrics-agent\", \"--serve\"]\n")

	merged, err := mergeHerdrConfig(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(merged) != string(existing) {
		t.Fatalf("unmanaged sidecars changed:\n%s", merged)
	}
}

func TestMergeHerdrConfigRejectsInvalidContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "invalid toml", content: "[ui\n"},
		{name: "missing end marker", content: managedSidecarBegin + "\n[[sidecar]]\ncommand = [\"herdr-pal\"]\n"},
		{name: "missing begin marker", content: "[[sidecar]]\ncommand = [\"herdr-pal\"]\n" + managedSidecarEnd + "\n"},
		{name: "duplicate markers", content: managedSidecarBlock + "\n" + managedSidecarBlock},
		{name: "reversed markers", content: managedSidecarEnd + "\n" + managedSidecarBegin + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mergeHerdrConfig([]byte(test.content)); err == nil {
				t.Fatal("mergeHerdrConfig() should fail")
			}
		})
	}
}
