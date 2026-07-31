package installer

import (
	"strings"
	"testing"
)

func TestMergeHerdrConfigAppendsManagedSidecarOnce(t *testing.T) {
	first, err := mergeHerdrConfig([]byte("[ui]\nmouse_capture = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := mergeHerdrConfig(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("merge is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Count(string(second), "[[sidecar]]") != 1 {
		t.Fatalf("sidecar count is not one:\n%s", second)
	}
	if !strings.Contains(string(second), managedSidecarBlock) {
		t.Fatalf("managed block missing:\n%s", second)
	}
}

func TestMergeHerdrConfigKeepsExistingUnmanagedPalSidecar(t *testing.T) {
	existing := []byte("[[sidecar]]\ncommand = [\n  \"herdr-pal\",\n]\n")

	merged, err := mergeHerdrConfig(existing)

	if err != nil {
		t.Fatal(err)
	}
	if string(merged) != string(existing) {
		t.Fatalf("existing sidecar changed:\n%s", merged)
	}
}

func TestMergeHerdrConfigPreservesOtherSidecarsAndSettings(t *testing.T) {
	existing := []byte("onboarding = false\n\n[[sidecar]]\ncommand = [\"metrics-agent\", \"--serve\"]\n")

	merged, err := mergeHerdrConfig(existing)

	if err != nil {
		t.Fatal(err)
	}
	text := string(merged)
	for _, want := range []string{"onboarding = false", `command = ["metrics-agent", "--serve"]`, managedSidecarBlock} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged config missing %q:\n%s", want, text)
		}
	}
}

func TestMergeHerdrConfigReplacesManagedBlock(t *testing.T) {
	existing := []byte("[ui]\nmouse_capture = true\n\n" + managedSidecarBegin + "\n[[sidecar]]\ncommand = [\"old-pal\"]\n" + managedSidecarEnd + "\n\n[advanced]\nscrollback_limit_bytes = 1234\n")

	merged, err := mergeHerdrConfig(existing)

	if err != nil {
		t.Fatal(err)
	}
	text := string(merged)
	if strings.Contains(text, "old-pal") || strings.Count(text, managedSidecarBegin) != 1 || strings.Count(text, managedSidecarEnd) != 1 {
		t.Fatalf("managed block was not replaced:\n%s", text)
	}
	for _, want := range []string{"mouse_capture = true", "scrollback_limit_bytes = 1234", `command = ["herdr-pal"]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged config missing %q:\n%s", want, text)
		}
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
