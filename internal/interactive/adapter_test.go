package interactive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/wecom"
)

const testTimeout = 2 * time.Second

func TestNewAdapterRejectsNilStreams(t *testing.T) {
	tests := []struct {
		name   string
		input  io.Reader
		output io.Writer
	}{
		{name: "nil input", output: io.Discard},
		{name: "nil output", input: strings.NewReader("")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := NewAdapter(test.input, test.output)
			if err == nil || adapter != nil {
				t.Fatalf("NewAdapter() = (%v, %v), want explicit error", adapter, err)
			}
		})
	}
}

func TestRunEmitsOneIncomingMessagePerInputLine(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close() })
	var output bytes.Buffer
	adapter, err := NewAdapter(reader, &output)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}

	runErrors := make(chan error, 1)
	go func() { runErrors <- adapter.Run(context.Background()) }()
	if _, err := io.WriteString(writer, "/ls\nhello agent\n\n"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	wantContent := []string{"/ls", "hello agent", ""}
	seenIDs := make(map[string]struct{}, len(wantContent)*2)
	for index, content := range wantContent {
		message := receiveEvent(t, adapter.Events())
		if message.UserID != UserID || message.BotID != BotID || message.ChatType != "single" {
			t.Fatalf("message[%d] identity = %#v", index, message)
		}
		if message.Content != content {
			t.Fatalf("message[%d].Content = %q, want %q", index, message.Content, content)
		}
		requestSequence := parseSequence(t, message.RequestID, "interactive-request-")
		messageSequence := parseSequence(t, message.MessageID, "interactive-message-")
		if requestSequence != index+1 || messageSequence != index+1 {
			t.Fatalf("message[%d] sequences = (%d, %d), want %d", index, requestSequence, messageSequence, index+1)
		}
		for _, id := range []string{message.RequestID, message.MessageID} {
			if _, exists := seenIDs[id]; exists {
				t.Fatalf("duplicate ID %q", id)
			}
			seenIDs[id] = struct{}{}
		}
	}

	if err := receiveError(t, runErrors); !errors.Is(err, ErrInputClosed) {
		t.Fatalf("Run() error = %v, want ErrInputClosed", err)
	}
	select {
	case _, open := <-adapter.Events():
		if !open {
			t.Fatal("Events() was closed after EOF")
		}
		t.Fatal("Events() produced an unexpected extra message")
	default:
	}
}

func TestRunWritesFixedBanner(t *testing.T) {
	var output bytes.Buffer
	adapter, err := NewAdapter(strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	if err := adapter.Run(context.Background()); !errors.Is(err, ErrInputClosed) {
		t.Fatalf("Run() error = %v, want ErrInputClosed", err)
	}
	const want = "Herdr Pal 交互模式\n输入 /ls 查看 Agent，按 Ctrl+C 或 Ctrl+D 退出。\n\nherdr-pal> "
	if output.String() != want {
		t.Fatalf("banner = %q, want %q", output.String(), want)
	}
}

func TestMarkdownMethodsWriteFixedBlocks(t *testing.T) {
	var output bytes.Buffer
	adapter, err := NewAdapter(strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	if err := adapter.RespondMarkdown(context.Background(), "request-1", "reply body"); err != nil {
		t.Fatalf("RespondMarkdown() error = %v", err)
	}
	if err := adapter.SendMarkdown(context.Background(), "notice body"); err != nil {
		t.Fatalf("SendMarkdown() error = %v", err)
	}
	const want = "\n[回复]\nreply body\n\nherdr-pal> \n[通知]\nnotice body\n\nherdr-pal> "
	if output.String() != want {
		t.Fatalf("markdown output = %q, want %q", output.String(), want)
	}
}

func TestMarkdownWritesRemainAtomicUnderConcurrency(t *testing.T) {
	writer := &interleavingWriter{}
	adapter, err := NewAdapter(strings.NewReader(""), writer)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}

	const goroutines = 32
	errorsChannel := make(chan error, goroutines)
	wantBlocks := make([]string, goroutines)
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		index := index
		content := fmt.Sprintf("marker-%02d-unique-body", index)
		label := "回复"
		if index%2 == 1 {
			label = "通知"
		}
		wantBlocks[index] = expectedMarkdownBlock(label, content)
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if index%2 == 0 {
				errorsChannel <- adapter.RespondMarkdown(context.Background(), fmt.Sprintf("request-%d", index), content)
				return
			}
			errorsChannel <- adapter.SendMarkdown(context.Background(), content)
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent markdown error = %v", err)
		}
	}

	got := writer.String()
	wantLength := 0
	for index, block := range wantBlocks {
		marker := fmt.Sprintf("marker-%02d-unique-body", index)
		if count := strings.Count(got, marker); count != 1 {
			t.Fatalf("marker %q count = %d, output = %q", marker, count, got)
		}
		if count := strings.Count(got, block); count != 1 {
			t.Fatalf("complete block %q count = %d, output = %q", block, count, got)
		}
		wantLength += len(block)
	}
	if len(got) != wantLength {
		t.Fatalf("output length = %d, want %d", len(got), wantLength)
	}
}

func TestMarkdownWritesUseOneWritePerBlockUnderConcurrency(t *testing.T) {
	writer := &recordingWriter{}
	adapter, err := NewAdapter(strings.NewReader(""), writer)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}

	const goroutines = 32
	errorsChannel := make(chan error, goroutines)
	wantBlocks := make(map[string]int, goroutines)
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		index := index
		content := fmt.Sprintf("single-write-marker-%02d", index)
		label := "回复"
		if index%2 == 1 {
			label = "通知"
		}
		wantBlocks[expectedMarkdownBlock(label, content)] = 1
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if index%2 == 0 {
				errorsChannel <- adapter.RespondMarkdown(context.Background(), fmt.Sprintf("request-%d", index), content)
				return
			}
			errorsChannel <- adapter.SendMarkdown(context.Background(), content)
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent markdown error = %v", err)
		}
	}

	chunks := writer.Chunks()
	if len(chunks) != goroutines {
		t.Fatalf("Write() calls = %d, want %d; chunks = %#v", len(chunks), goroutines, chunks)
	}
	gotBlocks := make(map[string]int, goroutines)
	for _, chunk := range chunks {
		gotBlocks[chunk]++
	}
	if len(gotBlocks) != goroutines {
		t.Fatalf("unique complete chunks = %d, want %d; chunks = %#v", len(gotBlocks), goroutines, chunks)
	}
	for block, wantCount := range wantBlocks {
		if gotCount := gotBlocks[block]; gotCount != wantCount {
			t.Fatalf("complete block %q count = %d, want %d; chunks = %#v", block, gotCount, wantCount, chunks)
		}
	}
}

func TestRunReturnsErrInputClosedOnEOF(t *testing.T) {
	adapter, err := NewAdapter(strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	if err := adapter.Run(context.Background()); !errors.Is(err, ErrInputClosed) {
		t.Fatalf("Run() error = %v, want ErrInputClosed", err)
	}
}

func TestRunReturnsContextCancellationWhileReaderBlocks(t *testing.T) {
	reader := newBlockingReader()
	t.Cleanup(reader.Release)
	adapter, err := NewAdapter(reader, io.Discard)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErrors := make(chan error, 1)
	go func() { runErrors <- adapter.Run(ctx) }()
	waitClosed(t, reader.entered)
	cancel()
	if err := receiveError(t, runErrors); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestRunReturnsBannerWriteFailure(t *testing.T) {
	root := errors.New("low-level banner failure")
	adapter, err := NewAdapter(strings.NewReader(""), &failAfterWriter{successfulCalls: 0, err: root})
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	runErr := adapter.Run(context.Background())
	if !errors.Is(runErr, root) {
		t.Fatalf("Run() error = %v, want root write error", runErr)
	}
}

func TestMarkdownWriteFailurePropagatesToRun(t *testing.T) {
	methods := []struct {
		name string
		call func(*Adapter) error
	}{
		{name: "respond", call: func(adapter *Adapter) error {
			return adapter.RespondMarkdown(context.Background(), "request-1", "sensitive reply")
		}},
		{name: "send", call: func(adapter *Adapter) error {
			return adapter.SendMarkdown(context.Background(), "sensitive notification")
		}},
	}
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			root := errors.New("low-level markdown failure")
			writer := newFailAfterWriter(1, root)
			reader := newBlockingReader()
			t.Cleanup(reader.Release)
			adapter, err := NewAdapter(reader, writer)
			if err != nil {
				t.Fatalf("NewAdapter() error = %v", err)
			}
			runErrors := make(chan error, 1)
			go func() { runErrors <- adapter.Run(context.Background()) }()
			waitClosed(t, writer.firstWrite)

			writeErr := method.call(adapter)
			if !errors.Is(writeErr, root) {
				t.Fatalf("markdown error = %v, want root write error", writeErr)
			}
			runErr := receiveError(t, runErrors)
			if runErr != writeErr {
				t.Fatalf("Run() error = %v, want same fatal error %v", runErr, writeErr)
			}
		})
	}
}

func TestRunReturnsSafeScannerError(t *testing.T) {
	const sensitiveLine = "secret-complete-message"
	root := errors.New("reader failed after " + sensitiveLine)
	reader := &errorAfterDataReader{data: []byte(sensitiveLine + "\n"), err: root}
	adapter, err := NewAdapter(reader, io.Discard)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	runErrors := make(chan error, 1)
	go func() { runErrors <- adapter.Run(context.Background()) }()
	message := receiveEvent(t, adapter.Events())
	if message.Content != sensitiveLine {
		t.Fatalf("message.Content = %q, want %q", message.Content, sensitiveLine)
	}
	runErr := receiveError(t, runErrors)
	if !errors.Is(runErr, root) {
		t.Fatalf("Run() error = %v, want scanner root error", runErr)
	}
	if strings.Contains(runErr.Error(), sensitiveLine) {
		t.Fatalf("Run() error leaked completed input: %q", runErr)
	}
}

func TestRunReturnsSafeErrorForOversizedInputLine(t *testing.T) {
	const sensitiveMarker = "oversized-sensitive-marker"
	input := sensitiveMarker + strings.Repeat("x", maxInputLineBytes+1)
	adapter, err := NewAdapter(strings.NewReader(input), io.Discard)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}

	runErr := adapter.Run(context.Background())
	if runErr == nil {
		t.Fatal("Run() error = nil, want Scanner token limit error")
	}
	if errors.Is(runErr, ErrInputClosed) {
		t.Fatalf("Run() error = %v, must not be ErrInputClosed", runErr)
	}
	if runErr.Error() != "交互输入读取失败" {
		t.Fatalf("Run() error = %q, want safe input read error", runErr)
	}
	if strings.Contains(runErr.Error(), sensitiveMarker) || strings.Contains(runErr.Error(), input) {
		t.Fatalf("Run() error leaked oversized input: %q", runErr)
	}
}

func TestRunPrefersInFlightFatalOverCanceledContext(t *testing.T) {
	root := errors.New("in-flight fatal before context cancellation")
	writer := newControlledFailureWriter(root, false)
	reader := newBlockingReader()
	t.Cleanup(reader.Release)
	adapter, err := NewAdapter(reader, writer)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErrors := make(chan error, 1)
	go func() { runErrors <- adapter.Run(ctx) }()
	waitClosed(t, writer.bannerWritten)
	waitClosed(t, reader.entered)
	writeErrors := make(chan error, 1)
	go func() { writeErrors <- adapter.SendMarkdown(context.Background(), "notification") }()
	waitClosed(t, writer.failureStarted)
	waitClosed(t, adapter.fatalPending)
	waitClosed(t, adapter.fatalReady)
	cancel()

	writeErr := receiveError(t, writeErrors)
	runErr := receiveError(t, runErrors)
	if !errors.Is(writeErr, root) || runErr != writeErr {
		t.Fatalf("errors = write:%v run:%v, want same root fatal", writeErr, runErr)
	}
}

func TestFatalStopsConcurrentQueuedMarkdownWrite(t *testing.T) {
	root := errors.New("first markdown failure")
	writer := newControlledFailureWriter(root, true)
	reader := newBlockingReader()
	t.Cleanup(reader.Release)
	adapter, err := NewAdapter(reader, writer)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	runErrors := make(chan error, 1)
	go func() { runErrors <- adapter.Run(context.Background()) }()
	waitClosed(t, writer.bannerWritten)
	waitClosed(t, reader.entered)

	firstErrors := make(chan error, 1)
	go func() { firstErrors <- adapter.RespondMarkdown(context.Background(), "request-1", "first") }()
	waitClosed(t, writer.failureStarted)
	secondStarted := make(chan struct{})
	secondErrors := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondErrors <- adapter.SendMarkdown(context.Background(), "second")
	}()
	waitClosed(t, secondStarted)
	writer.ReleaseFailure()

	firstErr := receiveError(t, firstErrors)
	secondErr := receiveError(t, secondErrors)
	runErr := receiveError(t, runErrors)
	if !errors.Is(firstErr, root) {
		t.Fatalf("first markdown error = %v, want root error", firstErr)
	}
	if secondErr != firstErr || runErr != firstErr {
		t.Fatalf("errors = first:%v second:%v run:%v, want same first fatal", firstErr, secondErr, runErr)
	}
	if calls := writer.Calls(); calls != 2 {
		t.Fatalf("Write() calls = %d, want banner plus first failed markdown only", calls)
	}
}

func TestFatalUnblocksInFlightEventDelivery(t *testing.T) {
	root := errors.New("event delivery output failure")
	writer := newControlledFailureWriter(root, false)
	reader := newGatedTerminalReader([]byte("late-event\n"))
	t.Cleanup(func() { reader.Release(nil) })
	adapter, err := NewAdapter(reader, writer)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	for index := 0; index < cap(adapter.events); index++ {
		adapter.events <- wecom.IncomingText{Content: "prefilled"}
	}

	runErrors := make(chan error, 1)
	go func() { runErrors <- adapter.Run(context.Background()) }()
	waitClosed(t, writer.bannerWritten)
	waitClosed(t, reader.waitingTerminal)
	waitFor(t, func() bool {
		if adapter.deliveryMu.TryLock() {
			adapter.deliveryMu.Unlock()
			return false
		}
		return true
	})

	writeErrors := make(chan error, 1)
	go func() { writeErrors <- adapter.SendMarkdown(context.Background(), "notification") }()
	waitClosed(t, writer.failureStarted)
	waitClosed(t, adapter.fatalPending)
	waitClosed(t, adapter.fatalReady)
	writeErr := receiveError(t, writeErrors)
	runErr := receiveError(t, runErrors)
	if !errors.Is(writeErr, root) || runErr != writeErr {
		t.Fatalf("errors = write:%v run:%v, want same root fatal", writeErr, runErr)
	}
	if len(adapter.Events()) != cap(adapter.events) {
		t.Fatalf("events length = %d, want unchanged full queue %d", len(adapter.Events()), cap(adapter.events))
	}
	for index := 0; index < cap(adapter.events); index++ {
		if message := <-adapter.Events(); message.Content != "prefilled" {
			t.Fatalf("event[%d] = %#v, want only prefilled events", index, message)
		}
	}
}

func TestRunPrefersInFlightFatalOverTerminalInput(t *testing.T) {
	tests := []struct {
		name        string
		terminalErr error
	}{
		{name: "EOF"},
		{name: "scanner error", terminalErr: errors.New("controlled scanner failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := errors.New("in-flight output failure")
			writer := newControlledFailureWriter(root, false)
			reader := newGatedTerminalReader(nil)
			t.Cleanup(func() { reader.Release(test.terminalErr) })
			adapter, err := NewAdapter(reader, writer)
			if err != nil {
				t.Fatalf("NewAdapter() error = %v", err)
			}

			runErrors := make(chan error, 1)
			go func() { runErrors <- adapter.Run(context.Background()) }()
			waitClosed(t, writer.bannerWritten)
			waitClosed(t, reader.waitingTerminal)
			writeErrors := make(chan error, 1)
			go func() { writeErrors <- adapter.SendMarkdown(context.Background(), "notification") }()
			waitClosed(t, writer.failureStarted)
			waitClosed(t, adapter.fatalPending)
			waitClosed(t, adapter.fatalReady)
			reader.Release(test.terminalErr)

			writeErr := receiveError(t, writeErrors)
			runErr := receiveError(t, runErrors)
			if !errors.Is(writeErr, root) || runErr != writeErr {
				t.Fatalf("errors = write:%v run:%v, want same root fatal", writeErr, runErr)
			}
		})
	}
}

func receiveEvent(t *testing.T, events <-chan wecom.IncomingText) wecom.IncomingText {
	t.Helper()
	select {
	case message := <-events:
		return message
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for input event")
		return wecom.IncomingText{}
	}
}

func receiveError(t *testing.T, errorsChannel <-chan error) error {
	t.Helper()
	select {
	case err := <-errorsChannel:
		return err
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for error")
		return nil
	}
}

func parseSequence(t *testing.T, id, prefix string) int {
	t.Helper()
	if id == "" || !strings.HasPrefix(id, prefix) {
		t.Fatalf("ID %q does not start with %q", id, prefix)
	}
	sequence, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if err != nil {
		t.Fatalf("ID %q sequence error = %v", id, err)
	}
	return sequence
}

func expectedMarkdownBlock(label, content string) string {
	return "\n[" + label + "]\n" + content + "\n\nherdr-pal> "
}

func waitClosed(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for signal")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("timed out waiting for condition")
}

type interleavingWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

type recordingWriter struct {
	mu     sync.Mutex
	chunks []string
}

func (w *recordingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chunks = append(w.chunks, string(data))
	return len(data), nil
}

func (w *recordingWriter) Chunks() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.chunks...)
}

func (w *interleavingWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		w.mu.Lock()
		_ = w.buffer.WriteByte(value)
		w.mu.Unlock()
		runtime.Gosched()
	}
	return len(data), nil
}

func (w *interleavingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

type failAfterWriter struct {
	mu              sync.Mutex
	successfulCalls int
	calls           int
	err             error
	firstWrite      chan struct{}
	firstWriteOnce  sync.Once
}

type controlledFailureWriter struct {
	mu             sync.Mutex
	calls          int
	err            error
	bannerWritten  chan struct{}
	failureStarted chan struct{}
	releaseFailure chan struct{}
	releaseOnce    sync.Once
}

func newControlledFailureWriter(err error, blockFailure bool) *controlledFailureWriter {
	writer := &controlledFailureWriter{
		err:            err,
		bannerWritten:  make(chan struct{}),
		failureStarted: make(chan struct{}),
		releaseFailure: make(chan struct{}),
	}
	if !blockFailure {
		writer.ReleaseFailure()
	}
	return writer
}

func (w *controlledFailureWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	call := w.calls
	w.mu.Unlock()
	switch call {
	case 1:
		close(w.bannerWritten)
		return len(data), nil
	case 2:
		close(w.failureStarted)
		<-w.releaseFailure
		return 0, w.err
	default:
		return len(data), nil
	}
}

func (w *controlledFailureWriter) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func (w *controlledFailureWriter) ReleaseFailure() {
	w.releaseOnce.Do(func() { close(w.releaseFailure) })
}

func newFailAfterWriter(successfulCalls int, err error) *failAfterWriter {
	return &failAfterWriter{successfulCalls: successfulCalls, err: err, firstWrite: make(chan struct{})}
}

func (w *failAfterWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.successfulCalls {
		w.firstWriteOnce.Do(func() { close(w.firstWrite) })
		return len(data), nil
	}
	return 0, w.err
}

type blockingReader struct {
	release  chan struct{}
	entered  chan struct{}
	once     sync.Once
	readOnce sync.Once
}

func newBlockingReader() *blockingReader {
	return &blockingReader{release: make(chan struct{}), entered: make(chan struct{})}
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func (r *blockingReader) Release() {
	r.once.Do(func() { close(r.release) })
}

type gatedTerminalReader struct {
	mu              sync.Mutex
	data            []byte
	terminalErr     error
	waitingTerminal chan struct{}
	release         chan struct{}
	waitingOnce     sync.Once
	releaseOnce     sync.Once
}

func newGatedTerminalReader(data []byte) *gatedTerminalReader {
	return &gatedTerminalReader{
		data:            append([]byte(nil), data...),
		waitingTerminal: make(chan struct{}),
		release:         make(chan struct{}),
	}
}

func (r *gatedTerminalReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	if len(r.data) > 0 {
		written := copy(buffer, r.data)
		r.data = r.data[written:]
		r.mu.Unlock()
		return written, nil
	}
	r.mu.Unlock()
	r.waitingOnce.Do(func() { close(r.waitingTerminal) })
	<-r.release
	r.mu.Lock()
	err := r.terminalErr
	r.mu.Unlock()
	if err == nil {
		return 0, io.EOF
	}
	return 0, err
}

func (r *gatedTerminalReader) Release(err error) {
	r.releaseOnce.Do(func() {
		r.mu.Lock()
		r.terminalErr = err
		r.mu.Unlock()
		close(r.release)
	})
}

type errorAfterDataReader struct {
	data []byte
	err  error
}

func (r *errorAfterDataReader) Read(buffer []byte) (int, error) {
	if len(r.data) > 0 {
		written := copy(buffer, r.data)
		r.data = r.data[written:]
		return written, nil
	}
	return 0, r.err
}
