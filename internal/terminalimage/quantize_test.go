package terminalimage

import (
	"image"
	"image/color"
	"reflect"
	"testing"
)

func TestHistogramQuantizeProducesDeterministicBoundedPalette(t *testing.T) {
	input := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < input.Bounds().Dy(); y++ {
		for x := 0; x < input.Bounds().Dx(); x++ {
			input.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 4), G: uint8(y * 8), B: uint8((x + y) * 2), A: 255,
			})
		}
	}

	first := histogramQuantize(input, 16)
	second := histogramQuantize(input, 16)
	if first.Bounds() != input.Bounds() || len(first.Palette) == 0 || len(first.Palette) > 16 {
		t.Fatalf("quantized image = bounds %v palette %d", first.Bounds(), len(first.Palette))
	}
	if !reflect.DeepEqual(first.Palette, second.Palette) || !reflect.DeepEqual(first.Pix, second.Pix) {
		t.Fatal("same input produced different quantized output")
	}
	for _, index := range first.Pix {
		if int(index) >= len(first.Palette) {
			t.Fatalf("palette index %d exceeds palette size %d", index, len(first.Palette))
		}
	}
}

func TestHistogramQuantizeClampsPaletteSize(t *testing.T) {
	input := image.NewRGBA(image.Rect(0, 0, 1, 1))
	input.SetRGBA(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	for _, maximum := range []int{-1, 0, 257} {
		result := histogramQuantize(input, maximum)
		if len(result.Palette) != 1 || result.Pix[0] != 0 {
			t.Fatalf("maxColors %d result = %#v", maximum, result)
		}
	}
}
