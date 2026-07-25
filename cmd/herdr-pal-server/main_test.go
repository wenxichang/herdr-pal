package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/serverapp"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func TestRunServerParsesConfigAndVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalVersion := version.Version
	t.Cleanup(func() { version.Version = originalVersion })
	version.Version = "v1.0.0"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, &stdout, &stderr, func(context.Context, serverapp.Options) error { return nil }); code != 0 {
		t.Fatalf("version code = %d", code)
	}
	if stdout.String() == "" {
		t.Fatal("version output is empty")
	}

	stdout.Reset()
	stderr.Reset()
	var gotDefault serverapp.Options
	code := run(context.Background(), nil, &stdout, &stderr, func(_ context.Context, options serverapp.Options) error {
		gotDefault = options
		return nil
	})
	wantDefault := filepath.Join(home, ".config", "herdr-pal", "server-config.json")
	if code != 0 || gotDefault.ConfigPath != wantDefault {
		t.Fatalf("default run() = %d, config path %q, want %q", code, gotDefault.ConfigPath, wantDefault)
	}

	stdout.Reset()
	stderr.Reset()
	var got serverapp.Options
	code = run(context.Background(), []string{"-config", "/tmp/server.json"}, &stdout, &stderr, func(_ context.Context, options serverapp.Options) error {
		got = options
		return nil
	})
	if code != 0 || got.ConfigPath != "/tmp/server.json" {
		t.Fatalf("run() = %d, options %#v", code, got)
	}
}

func TestRunServerMapsConfigAndRuntimeErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code int
	}{
		{name: "config", err: serverapp.ErrConfig, code: 2},
		{name: "runtime", err: errors.New("secret-sensitive"), code: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"-config", "/tmp/server.json"}, &stdout, &stderr, func(context.Context, serverapp.Options) error { return test.err })
			if code != test.code {
				t.Fatalf("code = %d", code)
			}
			if bytes.Contains(stderr.Bytes(), []byte("secret-sensitive")) {
				t.Fatalf("stderr leaked runtime error: %q", stderr.String())
			}
		})
	}
}
