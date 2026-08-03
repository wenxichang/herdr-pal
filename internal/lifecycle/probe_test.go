package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

func TestPublicProbeClassifiesCompatibleHerdr(t *testing.T) {
	client := &fakeProbeHerdr{}
	probe, err := NewPublicProbe(client)
	if err != nil {
		t.Fatalf("NewPublicProbe() error = %v", err)
	}
	result, err := probe.Probe(context.Background())
	if err != nil || !result.Alive || !result.Compatible {
		t.Fatalf("Probe() = %#v, %v", result, err)
	}
	if err := probe.VerifyReady(context.Background()); err != nil {
		t.Fatalf("VerifyReady() error = %v", err)
	}
	if client.snapshotCalls != 1 {
		t.Fatalf("Snapshot() calls = %d", client.snapshotCalls)
	}
}

func TestPublicProbeTreatsProtocolMismatchAsAlive(t *testing.T) {
	client := &fakeProbeHerdr{checkErr: fmt.Errorf("version: %w", herdr.ErrProtocolMismatch)}
	probe, err := NewPublicProbe(client)
	if err != nil {
		t.Fatalf("NewPublicProbe() error = %v", err)
	}
	result, err := probe.Probe(context.Background())
	if err != nil || !result.Alive || result.Compatible {
		t.Fatalf("Probe() = %#v, %v", result, err)
	}
}

func TestPublicProbeTreatsConnectionFailureAsUnavailable(t *testing.T) {
	client := &fakeProbeHerdr{checkErr: fmt.Errorf("dial: %w", herdr.ErrUnavailable)}
	probe, err := NewPublicProbe(client)
	if err != nil {
		t.Fatalf("NewPublicProbe() error = %v", err)
	}
	result, err := probe.Probe(context.Background())
	if result.Alive || result.Compatible || !errors.Is(err, herdr.ErrUnavailable) {
		t.Fatalf("Probe() = %#v, %v", result, err)
	}
}

type fakeProbeHerdr struct {
	checkErr      error
	snapshotErr   error
	snapshotCalls int
}

func (client *fakeProbeHerdr) CheckCompatible(context.Context) error {
	return client.checkErr
}

func (client *fakeProbeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	client.snapshotCalls++
	return herdr.Snapshot{}, client.snapshotErr
}
