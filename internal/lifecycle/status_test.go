package lifecycle

import (
	"sync"
	"testing"
)

func TestStatusStorePreservesConcurrentFieldUpdates(t *testing.T) {
	store := NewStatusStore(Status{State: StateStarting, Herdr: HerdrUnknown})
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})

	go func() {
		defer ready.Done()
		<-start
		store.Update(func(status *Status) {
			status.State = StateRunning
			status.WorkerPID = 4321
		})
	}()
	go func() {
		defer ready.Done()
		<-start
		store.Update(func(status *Status) {
			status.Herdr = HerdrHealthy
		})
	}()

	close(start)
	ready.Wait()
	got := store.Load()
	if got.State != StateRunning || got.Herdr != HerdrHealthy || got.WorkerPID != 4321 {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestStatusStoreReturnsSnapshots(t *testing.T) {
	store := NewStatusStore(Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 7})
	first := store.Load()
	first.WorkerPID = 99

	if got := store.Load(); got.WorkerPID != 7 {
		t.Fatalf("Load() returned shared state: %#v", got)
	}
}
