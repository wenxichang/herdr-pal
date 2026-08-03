package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/lifecycle"
)

func runLifecycleStart(ctx context.Context, options lifecycleCommandOptions) error {
	identity, err := app.ResolveRelayIdentity(ctx, app.Options{
		ConfigPath: options.ConfigPath,
		Getenv:     os.Getenv,
		Stderr:     options.Stderr,
	})
	if err != nil {
		return err
	}
	paths, err := lifecycle.DefaultRuntimePaths(identity.CanonicalEndpoint)
	if err != nil {
		return err
	}
	if err := lifecycle.PrepareRuntimeDirectories(paths); err != nil {
		return err
	}
	logFile, err := lifecycle.OpenLogFile(paths.LogFile, 0)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, nil))
	spawner, err := lifecycle.NewDetachedSpawner(logFile, os.Environ())
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	launcher, err := lifecycle.NewLauncher(lifecycle.LauncherOptions{
		Paths: paths, ConfigPath: options.ConfigPath,
		SocketPath: identity.CanonicalEndpoint, Executable: executable,
		Spawner: spawner, Logger: logger,
	})
	if err != nil {
		return err
	}
	return launcher.Start(ctx)
}

func runLifecycleSupervise(ctx context.Context, options lifecycleCommandOptions) error {
	paths, err := lifecycle.DefaultRuntimePaths(options.SocketPath)
	if err != nil {
		return err
	}
	if err := lifecycle.PrepareRuntimeDirectories(paths); err != nil {
		return err
	}
	logDestination := options.Stderr
	if logDestination == nil {
		logDestination = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logDestination, nil))
	herdrClient := herdr.NewClient(options.SocketPath, nil, 0)
	probe, err := lifecycle.NewPublicProbe(herdrClient)
	if err != nil {
		return err
	}
	status := lifecycle.NewStatusStore(lifecycle.Status{State: lifecycle.StateStarting, Herdr: lifecycle.HerdrUnknown})
	monitor, err := lifecycle.NewHerdrMonitor(probe, status, lifecycle.MonitorOptions{Logger: logger.With("component", "herdr_lifecycle")})
	if err != nil {
		return err
	}
	backoff, err := lifecycle.NewBackoff(time.Second, 30*time.Second)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	workerFactory, err := lifecycle.NewCommandWorkerFactory(lifecycle.CommandWorkerOptions{
		Executable: executable, ConfigPath: options.ConfigPath, SocketPath: options.SocketPath,
		Environment: os.Environ(), Stdout: options.Stdout, Stderr: logDestination,
	})
	if err != nil {
		return err
	}
	worker, err := lifecycle.NewWorkerSupervisor(workerFactory, status, lifecycle.WorkerSupervisorOptions{
		Backoff: backoff, Logger: logger.With("component", "worker_supervisor"),
	})
	if err != nil {
		return err
	}
	supervisor, err := lifecycle.NewSupervisor(lifecycle.SupervisorOptions{
		Paths: paths, Status: status, Monitor: monitor, Worker: worker,
		Control: lifecycle.NewControlServer(), Logger: logger.With("component", "pal_supervisor"),
	})
	if err != nil {
		return err
	}
	return supervisor.Run(ctx)
}

func runLifecycleWorker(ctx context.Context, options lifecycleCommandOptions) error {
	return app.RunManagedRelay(ctx, app.Options{
		ConfigPath: options.ConfigPath,
		Stdin:      options.Stdin,
		Stdout:     options.Stdout,
		Stderr:     options.Stderr,
		Getenv:     os.Getenv,
	})
}
