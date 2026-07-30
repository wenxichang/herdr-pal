// Package terminalimage 将安全 ANSI 终端页渲染为适合 IM 发送的 PNG8。
package terminalimage

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"strings"
	"sync"

	textimage "github.com/jiro4989/textimg/v3/image"
	"github.com/jiro4989/textimg/v3/parser"
	"github.com/mattn/go-runewidth"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

const (
	fontSize     = 16
	cellWidth    = 8
	cellHeight   = 17
	maxRows      = 100
	maxDimension = 16384
	maxPixels    = 2400 * 1700
	maxANSIBytes = 256 * 1024
	maxPNGBytes  = 512 * 1024
)

var (
	// ErrScreenTooLarge 表示终端页超过渲染尺寸上限。
	ErrScreenTooLarge = errors.New("终端图片尺寸超过限制")
	// ErrInputTooLarge 表示安全 ANSI 输入超过渲染器字节上限。
	ErrInputTooLarge = errors.New("终端 ANSI 输入超过限制")
	// ErrImageTooLarge 表示生成的 PNG 超过传输上限。
	ErrImageTooLarge = errors.New("终端 PNG 超过限制")

	//go:embed assets/NotoSansMonoCJKsc-Regular.otf
	embeddedFont []byte

	benchmarkPNG []byte
)

// Result 是一次终端图片渲染结果。
type Result struct {
	PNG    []byte
	Width  int
	Height int
}

// Renderer 持有可复用字体面，并串行保护 textimg 对字体面的访问。
type Renderer struct {
	mu   sync.Mutex
	face font.Face
}

// New 创建使用内嵌 Noto Sans Mono CJK SC 16px 字体的渲染器。
func New() (*Renderer, error) {
	parsed, err := opentype.Parse(embeddedFont)
	if err != nil {
		return nil, fmt.Errorf("解析内嵌终端字体: %w", err)
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: fontSize, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, fmt.Errorf("创建终端字体面: %w", err)
	}
	return &Renderer{face: face}, nil
}

// Render 把安全 ANSI 终端页渲染为最多 256 色的 PNG8。
func (r *Renderer) Render(ctx context.Context, safeANSI string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if r == nil || r.face == nil {
		return Result{}, errors.New("终端图片渲染器未初始化")
	}
	if len(safeANSI) > maxANSIBytes {
		return Result{}, ErrInputTooLarge
	}
	input, columns, rows, err := normalizeScreen(safeANSI)
	if err != nil {
		return Result{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	tokens, err := parser.Parse(input)
	if err != nil {
		return Result{}, fmt.Errorf("解析终端 ANSI: %w", err)
	}
	drawn := textimage.NewImage(&textimage.ImageParam{
		BaseWidth: columns, BaseHeight: rows,
		ForegroundColor: color.RGBA{R: 238, G: 238, B: 238, A: 255},
		BackgroundColor: color.RGBA{R: 10, G: 10, B: 10, A: 255},
		FontSize:        fontSize, FontFace: r.face,
	})
	if err := drawn.Draw(tokens); err != nil {
		return Result{}, fmt.Errorf("绘制终端图片: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	var intermediate bytes.Buffer
	if err := drawn.Encode(&intermediate, ".png"); err != nil {
		return Result{}, fmt.Errorf("编码 textimg 图片: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	decoded, err := png.Decode(bytes.NewReader(intermediate.Bytes()))
	if err != nil {
		return Result{}, fmt.Errorf("解码 textimg 图片: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	paletted := histogramQuantize(decoded, 256)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var output bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(&output, paletted); err != nil {
		return Result{}, fmt.Errorf("编码终端 PNG8: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if output.Len() > maxPNGBytes {
		return Result{}, ErrImageTooLarge
	}
	return Result{PNG: append([]byte(nil), output.Bytes()...), Width: columns * cellWidth, Height: rows * cellHeight}, nil
}

func normalizeScreen(safeANSI string) (string, int, int, error) {
	input := strings.ReplaceAll(safeANSI, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "")
	input = strings.TrimSuffix(input, "\n")
	plain := stripSGR(input)
	lines := strings.Split(plain, "\n")
	if plain == "" {
		lines = []string{""}
		input = " "
	}
	columns := 1
	for _, line := range lines {
		columns = max(columns, runewidth.StringWidth(line))
	}
	rows := len(lines)
	width, height := columns*cellWidth, rows*cellHeight
	if rows > maxRows || width > maxDimension || height > maxDimension || width > maxPixels/height {
		return "", 0, 0, ErrScreenTooLarge
	}
	return input, columns, rows, nil
}

func stripSGR(input string) string {
	var result strings.Builder
	result.Grow(len(input))
	for index := 0; index < len(input); {
		if input[index] != 0x1b || index+1 >= len(input) || input[index+1] != '[' {
			result.WriteByte(input[index])
			index++
			continue
		}
		end := index + 2
		for end < len(input) && (input[end] < 0x40 || input[end] > 0x7e) {
			end++
		}
		if end < len(input) && input[end] == 'm' {
			index = end + 1
			continue
		}
		result.WriteByte(input[index])
		index++
	}
	return result.String()
}
