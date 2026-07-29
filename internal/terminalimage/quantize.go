package terminalimage

import (
	"image"
	"image/color"
	"slices"
)

type colorSample struct {
	color color.RGBA
	count uint32
}

type colorBox struct {
	samples []colorSample
	count   uint64
	rangeR  uint8
	rangeG  uint8
	rangeB  uint8
}

func histogramQuantize(input image.Image, maxColors int) *image.Paletted {
	maxColors = max(1, min(maxColors, 256))
	bounds := input.Bounds()
	histogram := make(map[uint32]uint32, 1024)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			histogram[rgbaKeyAt(input, x, y)]++
		}
	}

	keys := make([]uint32, 0, len(histogram))
	for key := range histogram {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	samples := make([]colorSample, 0, len(keys))
	for _, key := range keys {
		samples = append(samples, colorSample{color: rgbaFromKey(key), count: histogram[key]})
	}
	palette := medianCutPalette(samples, maxColors)
	result := image.NewPaletted(bounds, palette)
	cache := make(map[uint32]uint8, len(histogram))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := (y - bounds.Min.Y) * result.Stride
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			key := rgbaKeyAt(input, x, y)
			index, ok := cache[key]
			if !ok {
				index = nearestPaletteIndex(rgbaFromKey(key), palette)
				cache[key] = index
			}
			result.Pix[row+x-bounds.Min.X] = index
		}
	}
	return result
}

func medianCutPalette(samples []colorSample, maxColors int) color.Palette {
	if len(samples) == 0 {
		return color.Palette{color.RGBA{A: 255}}
	}
	if len(samples) <= maxColors {
		slices.SortFunc(samples, compareColorSamples)
		palette := make(color.Palette, len(samples))
		for index, sample := range samples {
			palette[index] = sample.color
		}
		return palette
	}

	boxes := []colorBox{newColorBox(samples)}
	for len(boxes) < maxColors {
		boxIndex := splittableBoxIndex(boxes)
		if boxIndex < 0 {
			break
		}
		left, right := splitColorBox(boxes[boxIndex])
		boxes[boxIndex] = left
		boxes = append(boxes, right)
	}
	slices.SortFunc(boxes, func(left, right colorBox) int {
		if left.count > right.count {
			return -1
		}
		if left.count < right.count {
			return 1
		}
		return compareRGBA(representativeColor(left), representativeColor(right))
	})
	palette := make(color.Palette, len(boxes))
	for index, box := range boxes {
		palette[index] = representativeColor(box)
	}
	return palette
}

func newColorBox(samples []colorSample) colorBox {
	box := colorBox{samples: samples}
	if len(samples) == 0 {
		return box
	}
	minR, maxR := samples[0].color.R, samples[0].color.R
	minG, maxG := samples[0].color.G, samples[0].color.G
	minB, maxB := samples[0].color.B, samples[0].color.B
	for _, sample := range samples {
		box.count += uint64(sample.count)
		minR, maxR = min(minR, sample.color.R), max(maxR, sample.color.R)
		minG, maxG = min(minG, sample.color.G), max(maxG, sample.color.G)
		minB, maxB = min(minB, sample.color.B), max(maxB, sample.color.B)
	}
	box.rangeR = maxR - minR
	box.rangeG = maxG - minG
	box.rangeB = maxB - minB
	return box
}

func splittableBoxIndex(boxes []colorBox) int {
	bestIndex := -1
	var bestScore uint64
	for index, box := range boxes {
		if len(box.samples) < 2 {
			continue
		}
		colorRange := uint64(max(box.rangeR, max(box.rangeG, box.rangeB)))
		score := colorRange * box.count
		if score > bestScore {
			bestIndex, bestScore = index, score
		}
	}
	return bestIndex
}

func splitColorBox(box colorBox) (colorBox, colorBox) {
	axis := 0
	if box.rangeG > box.rangeR && box.rangeG >= box.rangeB {
		axis = 1
	} else if box.rangeB > box.rangeR && box.rangeB > box.rangeG {
		axis = 2
	}
	slices.SortFunc(box.samples, func(left, right colorSample) int {
		leftValue, rightValue := colorAxisValue(left.color, axis), colorAxisValue(right.color, axis)
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
		return compareRGBA(left.color, right.color)
	})
	half := box.count / 2
	var accumulated uint64
	splitIndex := 1
	for index, sample := range box.samples[:len(box.samples)-1] {
		accumulated += uint64(sample.count)
		splitIndex = index + 1
		if accumulated >= half {
			break
		}
	}
	return newColorBox(box.samples[:splitIndex:splitIndex]), newColorBox(box.samples[splitIndex:])
}

func representativeColor(box colorBox) color.RGBA {
	var red, green, blue, alpha uint64
	for _, sample := range box.samples {
		count := uint64(sample.count)
		red += uint64(sample.color.R) * count
		green += uint64(sample.color.G) * count
		blue += uint64(sample.color.B) * count
		alpha += uint64(sample.color.A) * count
	}
	if box.count == 0 {
		return color.RGBA{A: 255}
	}
	return color.RGBA{R: uint8(red / box.count), G: uint8(green / box.count), B: uint8(blue / box.count), A: uint8(alpha / box.count)}
}

func nearestPaletteIndex(value color.RGBA, palette color.Palette) uint8 {
	bestIndex := 0
	bestDistance := ^uint64(0)
	for index, entry := range palette {
		candidate := color.RGBAModel.Convert(entry).(color.RGBA)
		distance := channelDistance(value.R, candidate.R) + channelDistance(value.G, candidate.G) +
			channelDistance(value.B, candidate.B) + channelDistance(value.A, candidate.A)
		if distance < bestDistance {
			bestIndex, bestDistance = index, distance
		}
	}
	return uint8(bestIndex)
}

func channelDistance(left, right uint8) uint64 {
	delta := int64(left) - int64(right)
	return uint64(delta * delta)
}

func rgbaKeyAt(input image.Image, x, y int) uint32 {
	value := color.RGBAModel.Convert(input.At(x, y)).(color.RGBA)
	return uint32(value.R)<<24 | uint32(value.G)<<16 | uint32(value.B)<<8 | uint32(value.A)
}

func rgbaFromKey(key uint32) color.RGBA {
	return color.RGBA{R: uint8(key >> 24), G: uint8(key >> 16), B: uint8(key >> 8), A: uint8(key)}
}

func colorAxisValue(value color.RGBA, axis int) uint8 {
	switch axis {
	case 0:
		return value.R
	case 1:
		return value.G
	default:
		return value.B
	}
}

func compareColorSamples(left, right colorSample) int {
	if left.count > right.count {
		return -1
	}
	if left.count < right.count {
		return 1
	}
	return compareRGBA(left.color, right.color)
}

func compareRGBA(left, right color.RGBA) int {
	leftKey := uint32(left.R)<<24 | uint32(left.G)<<16 | uint32(left.B)<<8 | uint32(left.A)
	rightKey := uint32(right.R)<<24 | uint32(right.G)<<16 | uint32(right.B)<<8 | uint32(right.A)
	if leftKey < rightKey {
		return -1
	}
	if leftKey > rightKey {
		return 1
	}
	return 0
}
