package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/processlock"
)

func TestLauncherStartsSupervisorAndWaitsUntilRunning(t *testing.T) {
	harness := newLauncherHarness()
	launcher := harness.New(t)
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if harness.Spawns() != 1 {
		t.Fatalf("spawns = %d", harness.Spawns())
	}
}

func TestLauncherTreatsHealthySupervisorAsSuccess(t *testing.T) {
	harness := newLauncherHarness()
	harness.ownerHeld = true
	harness.statuses = []Status{{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 8}}
	launcher := harness.New(t)
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if harness.Spawns() != 0 {
		t.Fatalf("spawns = %d", harness.Spawns())
	}
}

func TestLauncherWaitsForSupervisorToRecoverFromHerdrGrace(t *testing.T) {
	harness := newLauncherHarness()
	harness.ownerHeld = true
	harness.statuses = []Status{
		{State: StateHerdrGrace, Herdr: HerdrUnavailable, WorkerPID: 8},
		{State: StateHerdrGrace, Herdr: HerdrUnavailable, WorkerPID: 8},
		{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 8},
	}
	launcher := harness.New(t)
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if harness.Spawns() != 0 || harness.waitCalls != 2 {
		t.Fatalf("spawns=%d waits=%d", harness.Spawns(), harness.waitCalls)
	}
}

func TestLauncherTakesOverAfterGraceSupervisorReleasesLock(t *testing.T) {
	harness := newLauncherHarness()
	harness.ownerHeld = true
	harness.statuses = []Status{{State: StateHerdrGrace, Herdr: HerdrUnavailable, WorkerPID: 8}}
	harness.afterWait = func() { harness.setOwnerHeld(false) }
	launcher := harness.New(t)
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if harness.Spawns() != 1 {
		t.Fatalf("spawns = %d", harness.Spawns())
	}
}

func TestLauncherRejectsUnresponsiveLockedSupervisor(t *testing.T) {
	harness := newLauncherHarness()
	harness.ownerHeld = true
	harness.statusErr = ErrControlUnavailable
	launcher := harness.New(t)
	err := launcher.Start(context.Background())
	if !errors.Is(err, ErrSupervisorUnresponsive) {
		t.Fatalf("Start() error = %v", err)
	}
	if harness.Spawns() != 0 {
		t.Fatalf("spawns = %d", harness.Spawns())
	}
}

func TestConcurrentLaunchersSpawnOneSupervisor(t *testing.T) {
	directory := t.TempDir()
	paths := RuntimePaths{
		StartLock:     filepath.Join(directory, "start.lock"),
		OwnerLock:     filepath.Join(directory, "owner.lock"),
		ControlSocket: filepath.Join(directory, "control.sock"),
	}
	spawner := &lockingSupervisorSpawner{ownerPath: paths.OwnerLock}
	control := staticStatusClient{status: Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 10}}
	newLauncher := func() *Launcher {
		launcher, err := NewLauncher(LauncherOptions{
			Paths: paths, ConfigPath: "/config.json", SocketPath: "/herdr.sock", Executable: "/herdr-pal",
			AcquireLock: func(path string) (LifecycleLock, error) { return processlock.Acquire(path) },
			Control:     control, Spawner: spawner,
			StartTimeout: time.Second, PollInterval: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewLauncher() error = %v", err)
		}
		return launcher
	}
	first := newLauncher()
	second := newLauncher()
	results := make(chan error, 2)
	go func() { results <- first.Start(context.Background()) }()
	go func() { results <- second.Start(context.Background()) }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Start() error = %v", err)
		}
	}
	t.Cleanup(spawner.Release)
	if spawner.Spawns() != 1 {
		t.Fatalf("spawns = %d, want 1", spawner.Spawns())
	}
}

type launcherHarness struct {
	mu         sync.Mutex
	ownerHeld  bool
	startHeld  bool
	statuses   []Status
	statusErr  error
	statusCall int
	spawnCount int
	waitCalls  int
	now        time.Time
	afterWait  func()
}

func newLauncherHarness() *launcherHarness {
	return &launcherHarness{now: time.Unix(0, 0)}
}

func (harness *launcherHarness) New(t *testing.T) *Launcher {
	t.Helper()
	launcher, err := NewLauncher(LauncherOptions{
		Paths:      RuntimePaths{StartLock: "/start.lock", OwnerLock: "/owner.lock", ControlSocket: "/control.sock"},
		ConfigPath: "/config.json", SocketPath: "/herdr.sock", Executable: "/herdr-pal",
		AcquireLock:  harness.Acquire,
		Control:      harness,
		Spawner:      harness,
		StartTimeout: 5 * time.Second,
		PollInterval: time.Second,
		Now:          harness.Now,
		Wait:         harness.Wait,
	})
	if err != nil {
		t.Fatalf("NewLauncher() error = %v", err)
	}
	return launcher
}

func (harness *launcherHarness) Acquire(path string) (LifecycleLock, error) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	switch path {
	case "/start.lock":
		if harness.startHeld {
			return nil, processlock.ErrAlreadyRunning
		}
		harness.startHeld = true
		return &callbackLock{release: func() {
			harness.mu.Lock()
			harness.startHeld = false
			harness.mu.Unlock()
		}}, nil
	case "/owner.lock":
		if harness.ownerHeld {
			return nil, processlock.ErrAlreadyRunning
		}
		return &callbackLock{}, nil
	default:
		return nil, errors.New("unexpected lock path")
	}
}

func (harness *launcherHarness) Status(context.Context, string) (Status, error) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.statusErr != nil {
		return Status{}, harness.statusErr
	}
	if len(harness.statuses) == 0 {
		return Status{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 7}, nil
	}
	index := harness.statusCall
	if index >= len(harness.statuses) {
		index = len(harness.statuses) - 1
	} else {
		harness.statusCall++
	}
	return harness.statuses[index], nil
}

func (harness *launcherHarness) Spawn(SupervisorCommand) error {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.spawnCount++
	harness.ownerHeld = true
	harness.statuses = []Status{{State: StateRunning, Herdr: HerdrHealthy, WorkerPID: 7}}
	harness.statusCall = 0
	return nil
}

func (harness *launcherHarness) Wait(context.Context, time.Duration) error {
	harness.mu.Lock()
	harness.waitCalls++
	harness.now = harness.now.Add(time.Second)
	afterWait := harness.afterWait
	harness.afterWait = nil
	harness.mu.Unlock()
	if afterWait != nil {
		afterWait()
	}
	return nil
}

func (harness *launcherHarness) Now() time.Time {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.now
}

func (harness *launcherHarness) Spawns() int {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.spawnCount
}

func (harness *launcherHarness) setOwnerHeld(held bool) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.ownerHeld = held
}

type callbackLock struct {
	once    sync.Once
	release func()
}

func (lock *callbackLock) Release() error {
	lock.once.Do(func() {
		if lock.release != nil {
			lock.release()
		}
	})
	return nil
}

type staticStatusClient struct{ status Status }

func (client staticStatusClient) Status(context.Context, string) (Status, error) {
	return client.status, nil
}

type lockingSupervisorSpawner struct {
	mu        sync.Mutex
	ownerPath string
	owner     LifecycleLock
	spawns    int
}

func (spawner *lockingSupervisorSpawner) Spawn(SupervisorCommand) error {
	spawner.mu.Lock()
	defer spawner.mu.Unlock()
	if spawner.owner != nil {
		return errors.New("duplicate spawn")
	}
	lock, err := processlock.Acquire(spawner.ownerPath)
	if err != nil {
		return err
	}
	spawner.owner = lock
	spawner.spawns++
	return nil
}

func (spawner *lockingSupervisorSpawner) Spawns() int {
	spawner.mu.Lock()
	defer spawner.mu.Unlock()
	return spawner.spawns
}

func (spawner *lockingSupervisorSpawner) Release() {
	spawner.mu.Lock()
	defer spawner.mu.Unlock()
	if spawner.owner != nil {
		_ = spawner.owner.Release()
		spawner.owner = nil
	}
}
