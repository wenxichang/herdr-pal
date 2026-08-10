package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestUserExecutorSerializesSameUserInArrivalOrder(t *testing.T) {
	executor := NewUserExecutor(4)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var order []string
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- executor.Submit(context.Background(), "user-a", func(context.Context) error {
			close(firstStarted)
			<-releaseFirst
			mu.Lock()
			order = append(order, "first")
			mu.Unlock()
			return nil
		})
	}()
	<-firstStarted
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- executor.Submit(context.Background(), "user-a", func(context.Context) error {
			close(secondStarted)
			mu.Lock()
			order = append(order, "second")
			mu.Unlock()
			return nil
		})
	}()
	select {
	case <-secondStarted:
		t.Fatal("second task started before first completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("order = %#v", order)
	}
}

func TestUserExecutorRunsDifferentUsersConcurrently(t *testing.T) {
	executor := NewUserExecutor(2)
	release := make(chan struct{})
	started := make(chan string, 2)
	var wg sync.WaitGroup
	for _, userID := range []string{"user-a", "user-b"} {
		wg.Add(1)
		go func(userID string) {
			defer wg.Done()
			if err := executor.Submit(context.Background(), userID, func(context.Context) error {
				started <- userID
				<-release
				return nil
			}); err != nil {
				t.Errorf("Submit(%s) error = %v", userID, err)
			}
		}(userID)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different users did not run concurrently")
		}
	}
	close(release)
	wg.Wait()
}

func TestUserExecutorEnqueuePreservesSubmissionOrder(t *testing.T) {
	executor := NewUserExecutor(2)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	done := make(chan string, 2)

	if err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		done <- "first"
		return nil
	}); err != nil {
		t.Fatalf("Enqueue(first) error = %v", err)
	}
	<-firstStarted
	if err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error {
		done <- "second"
		return nil
	}); err != nil {
		t.Fatalf("Enqueue(second) error = %v", err)
	}
	select {
	case value := <-done:
		t.Fatalf("task %q completed before first release", value)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	for _, want := range []string{"first", "second"} {
		select {
		case got := <-done:
			if got != want {
				t.Fatalf("completion order = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestUserExecutorEnqueueOverflowRunsAfterAcceptedTasks(t *testing.T) {
	executor := NewUserExecutor(1)
	started := make(chan struct{})
	release := make(chan struct{})
	order := make(chan string, 2)
	if err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error {
		close(started)
		<-release
		order <- "accepted"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error { return nil }); !errors.Is(err, ErrUserQueueFull) {
		t.Fatalf("Enqueue(full) error = %v", err)
	}
	if err := executor.EnqueueOverflow(context.Background(), "user-a", func(context.Context) error {
		order <- "overflow"
		return nil
	}); err != nil {
		t.Fatalf("EnqueueOverflow() error = %v", err)
	}
	if err := executor.EnqueueOverflow(context.Background(), "user-a", func(context.Context) error { return nil }); !errors.Is(err, ErrUserQueueFull) {
		t.Fatalf("second EnqueueOverflow() error = %v, want full", err)
	}
	close(release)
	for _, want := range []string{"accepted", "overflow"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("execution order = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestUserExecutorEnqueueCanceledTaskIsSkippedAndCapacityReleased(t *testing.T) {
	executor := NewUserExecutor(2)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	runCanceled := make(chan struct{}, 1)
	if err := executor.Enqueue(canceledContext, "user-a", func(context.Context) error {
		runCanceled <- struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Enqueue(canceled) error = %v", err)
	}
	if err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error { return nil }); !errors.Is(err, ErrUserQueueFull) {
		t.Fatalf("Enqueue(full) error = %v", err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		err := executor.Enqueue(context.Background(), "user-a", func(context.Context) error { return nil })
		if err == nil {
			break
		}
		if !errors.Is(err, ErrUserQueueFull) || time.Now().After(deadline) {
			t.Fatalf("capacity was not released: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-runCanceled:
		t.Fatal("canceled task was executed")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestUserExecutorRejectsQueueOverflow(t *testing.T) {
	executor := NewUserExecutor(1)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- executor.Submit(context.Background(), "user-a", func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	if err := executor.Submit(context.Background(), "user-a", func(context.Context) error { return nil }); !errors.Is(err, ErrUserQueueFull) {
		t.Fatalf("Submit(overflow) error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
