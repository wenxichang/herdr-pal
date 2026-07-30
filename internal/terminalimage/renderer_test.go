package terminalimage

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/font/sfnt"
)

func TestRendererProducesIndexedPNGWithCJKAndANSI(t *testing.T) {
	renderer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	result, err := renderer.Render(context.Background(), "\x1b[7m当前选项 中文 ABC 123\x1b[0m")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(result.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.(*image.Paletted); !ok {
		t.Fatalf("decoded = %T", decoded)
	}
	if result.Width <= 0 || result.Height != cellHeight {
		t.Fatalf("size = %dx%d", result.Width, result.Height)
	}
	if decoded.Bounds().Dx() != result.Width || decoded.Bounds().Dy() != result.Height {
		t.Fatalf("decoded bounds = %v, result = %dx%d", decoded.Bounds(), result.Width, result.Height)
	}
}

func TestRendererUsesEmbeddedFontForRepresentativeTerminalRunes(t *testing.T) {
	fontData, err := sfnt.Parse(embeddedFont)
	if err != nil {
		t.Fatal(err)
	}
	var buffer sfnt.Buffer
	for _, current := range "ABC中文▣┃✓↑↓∞—•" {
		glyph, err := fontData.GlyphIndex(&buffer, current)
		if err != nil {
			t.Fatal(err)
		}
		if glyph == 0 {
			t.Fatalf("embedded font missing %q", current)
		}
	}
}

func TestRendererRejectsUnboundedScreen(t *testing.T) {
	renderer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		strings.Repeat(strings.Repeat("x", 301)+"\n", maxRows-1) + strings.Repeat("x", 301),
		strings.Repeat("line\n", maxRows) + "line",
	} {
		if _, err := renderer.Render(context.Background(), input); !errors.Is(err, ErrScreenTooLarge) {
			t.Fatalf("Render() error = %v", err)
		}
	}
}

func TestNormalizeScreenAllowsWideShortPageWithinPixelBudget(t *testing.T) {
	input := strings.Repeat(strings.Repeat("x", 301)+"\n", 63) + strings.Repeat("x", 301)
	_, columns, rows, err := normalizeScreen(input)
	if err != nil {
		t.Fatalf("normalizeScreen() error = %v", err)
	}
	if columns != 301 || rows != 64 {
		t.Fatalf("normalizeScreen() size = %dx%d, want 301x64", columns, rows)
	}
}

func TestRendererRejectsOversizedANSIInput(t *testing.T) {
	renderer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Repeat("\x1b[31m", maxANSIBytes/5+1) + "x"
	if _, err := renderer.Render(context.Background(), input); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("Render() error = %v", err)
	}
}

func TestRendererHonorsCancelledContext(t *testing.T) {
	renderer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := renderer.Render(ctx, "content"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Render() error = %v", err)
	}
}

func BenchmarkRenderer(b *testing.B) {
	renderer, err := New()
	if err != nil {
		b.Fatal(err)
	}
	input := strings.Repeat("\x1b[38;5;45m终端输出 ABC 123\x1b[0m\n", 50)
	b.ResetTimer()
	for range b.N {
		result, err := renderer.Render(context.Background(), input)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkPNG = result.PNG
	}
}
