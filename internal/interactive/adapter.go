// Package interactive 提供把本机 stdin/stdout 适配为单用户 IM 会话的运行时。
package interactive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/wenxichang/herdr-pal/internal/im"
)

const (
	// UserID 是交互模式唯一允许的本地用户标识。
	UserID = "interactive-local"
	// BotID 是交互模式写入入站消息的本地适配器标识。
	BotID = "interactive-local"

	eventsCapacity    = 64
	readerCapacity    = 1
	initialBufferSize = 64 * 1024
	maxInputLineBytes = 1 << 20
	banner            = "Herdr Pal 交互模式\n输入 /ls 查看 Agent，按 Ctrl+C 或 Ctrl+D 退出。\n\nherdr-pal> "
)

var (
	// ErrInputClosed 表示 stdin 已到达 EOF，应用应执行正常退出。
	ErrInputClosed = errors.New("交互输入已关闭")
	errNilStream   = errors.New("交互适配器输入输出不能为空")
	errRepeatedRun = errors.New("交互适配器只能运行一次")
)

// Adapter 把本机行输入转换为单用户入站消息，并串行输出回复与通知。
//
// Adapter 只支持一次 Run；Run 退出时不会关闭 Events 返回的消息流。
type Adapter struct {
	input        io.Reader
	output       io.Writer
	events       chan im.IncomingText
	fatalPending chan struct{}
	fatalReady   chan struct{}
	stop         chan struct{}

	writeMu    sync.Mutex
	deliveryMu sync.Mutex
	runMu      sync.Mutex
	run        bool
	fatalErr   error

	fatalOnce sync.Once
	stopOnce  sync.Once
}

// NewAdapter 创建本地交互适配器。
func NewAdapter(input io.Reader, output io.Writer) (*Adapter, error) {
	if input == nil || output == nil {
		return nil, errNilStream
	}
	return &Adapter{
		input:        input,
		output:       output,
		events:       make(chan im.IncomingText, eventsCapacity),
		fatalPending: make(chan struct{}),
		fatalReady:   make(chan struct{}),
		stop:         make(chan struct{}),
	}, nil
}

// Run 读取 stdin 并产生本地单聊消息，直到取消、EOF 或 I/O 失败。
func (a *Adapter) Run(ctx context.Context) error {
	if a == nil {
		return errNilStream
	}
	if err := a.currentFatalError(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.startRun(); err != nil {
		return err
	}
	defer a.stopOnce.Do(func() { close(a.stop) })

	if err := a.write(ctx, banner); err != nil {
		return a.resolveTerminal(err)
	}
	if err := a.runtimeError(ctx); err != nil {
		return err
	}

	results := make(chan scanResult, readerCapacity)
	go a.scan(results)

	sequence := uint64(0)
	for {
		if err := a.runtimeError(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return a.resolveTerminal(ctx.Err())
		case <-a.fatalPending:
			return a.currentFatalError()
		case result := <-results:
			if err := a.runtimeError(ctx); err != nil {
				return err
			}
			if result.err != nil {
				return a.resolveTerminal(newSafeError("交互输入读取失败", result.err))
			}
			if result.eof {
				return a.resolveTerminal(ErrInputClosed)
			}
			if err := a.runtimeError(ctx); err != nil {
				return err
			}

			sequence++
			message := im.IncomingText{
				RequestID: fmt.Sprintf("interactive-request-%d", sequence),
				MessageID: fmt.Sprintf("interactive-message-%d", sequence),
				BotID:     BotID,
				UserID:    UserID,
				ChatType:  "single",
				Content:   result.line,
			}
			if err := a.runtimeError(ctx); err != nil {
				return err
			}
			if err := a.sendEvent(ctx, message); err != nil {
				return err
			}
			if err := a.runtimeError(ctx); err != nil {
				return err
			}
		}
	}
}

// Events 返回单向入站消息流。
func (a *Adapter) Events() <-chan im.IncomingText {
	if a == nil {
		return nil
	}
	return a.events
}

// RespondMarkdown 输出带“回复”标签的回调消息。
func (a *Adapter) RespondMarkdown(ctx context.Context, requestID, content string) error {
	return a.write(ctx, markdownBlock("回复", content))
}

// SendMarkdown 输出带“通知”标签的主动消息。
func (a *Adapter) SendMarkdown(ctx context.Context, content string) error {
	return a.write(ctx, markdownBlock("通知", content))
}

func (a *Adapter) startRun() error {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.run {
		return errRepeatedRun
	}
	a.run = true
	return nil
}

func (a *Adapter) scan(results chan<- scanResult) {
	scanner := bufio.NewScanner(a.input)
	scanner.Buffer(make([]byte, initialBufferSize), maxInputLineBytes)
	for scanner.Scan() {
		if !a.sendScanResult(results, scanResult{line: scanner.Text()}) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		a.sendScanResult(results, scanResult{err: err})
		return
	}
	a.sendScanResult(results, scanResult{eof: true})
}

func (a *Adapter) sendScanResult(results chan<- scanResult, result scanResult) bool {
	select {
	case results <- result:
		return true
	case <-a.stop:
		return false
	}
}

func (a *Adapter) write(ctx context.Context, value string) error {
	if a == nil {
		return errNilStream
	}
	if err := a.currentFatalError(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if err := a.currentFatalError(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	written, err := io.WriteString(a.output, value)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	if err == nil {
		return nil
	}

	safeErr := newSafeError("交互输出写入失败", err)
	a.recordFatal(safeErr)
	return safeErr
}

func (a *Adapter) sendEvent(ctx context.Context, message im.IncomingText) error {
	if err := a.runtimeError(ctx); err != nil {
		return err
	}

	// 持锁发送建立事件完成与 fatalErr 记录的线性顺序；pending 信号避免队列满时死锁。
	a.deliveryMu.Lock()
	select {
	case <-a.fatalPending:
		a.deliveryMu.Unlock()
		return a.currentFatalError()
	default:
	}

	select {
	case a.events <- message:
		a.deliveryMu.Unlock()
		if err := a.runtimeError(ctx); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		a.deliveryMu.Unlock()
		return a.resolveTerminal(ctx.Err())
	case <-a.fatalPending:
		a.deliveryMu.Unlock()
		return a.currentFatalError()
	}
}

func (a *Adapter) runtimeError(ctx context.Context) error {
	if err := a.currentFatalError(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return a.resolveTerminal(err)
	}
	return nil
}

func (a *Adapter) currentFatalError() error {
	select {
	case <-a.fatalPending:
		<-a.fatalReady
		return a.fatalErr
	default:
		return nil
	}
}

func (a *Adapter) resolveTerminal(fallback error) error {
	select {
	case <-a.fatalPending:
		return a.currentFatalError()
	default:
		return fallback
	}
}

func (a *Adapter) recordFatal(err error) {
	a.fatalOnce.Do(func() {
		// 先唤醒持锁的事件发送，再等待它建立完成投递与 fatal 记录的顺序。
		close(a.fatalPending)
		a.deliveryMu.Lock()
		a.fatalErr = err
		close(a.fatalReady)
		a.deliveryMu.Unlock()
	})
}

func markdownBlock(label, content string) string {
	return "\n[" + label + "]\n" + content + "\n\nherdr-pal> "
}

type scanResult struct {
	line string
	err  error
	eof  bool
}

type safeError struct {
	message string
	cause   error
}

func newSafeError(message string, cause error) error {
	return &safeError{message: message, cause: cause}
}

func (e *safeError) Error() string { return e.message }

func (e *safeError) Unwrap() error { return e.cause }
