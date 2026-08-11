package process

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunStopsSiblingAfterServerFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("listener failed")
	siblingStopped := make(chan struct{})
	err := Run(
		t.Context(),
		func(context.Context) error { return want },
		func(ctx context.Context) error {
			<-ctx.Done()
			close(siblingStopped)
			return nil
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
	select {
	case <-siblingStopped:
	case <-time.After(time.Second):
		t.Fatal("sibling server was not stopped")
	}
}

func TestRunStopsOnParentCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
