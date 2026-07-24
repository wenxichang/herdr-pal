package serverapp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/im"
)

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	if _, err := newLogger(&bytes.Buffer{}, "verbose"); err == nil {
		t.Fatal("newLogger() should reject unknown level")
	}
}

func TestRunServerComponentsStopsHTTPAndWeComOnContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	weCom := &fakeWeComRuntime{events: make(chan im.IncomingText)}
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = runServerComponents(ctx, weCom, nil, httpServer, listener)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runServerComponents() error = %v", err)
	}
	select {
	case <-weCom.stopped:
	case <-time.After(time.Second):
		t.Fatal("WeCom runtime did not stop")
	}
}

type fakeWeComRuntime struct {
	events  chan im.IncomingText
	stopped chan struct{}
}

func (runtime *fakeWeComRuntime) Run(ctx context.Context) error {
	if runtime.stopped == nil {
		runtime.stopped = make(chan struct{})
	}
	<-ctx.Done()
	close(runtime.stopped)
	return ctx.Err()
}

func (runtime *fakeWeComRuntime) Events() <-chan im.IncomingText { return runtime.events }
