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
