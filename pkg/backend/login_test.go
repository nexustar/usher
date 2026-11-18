package backend

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blockedFlow struct{}

func (blockedFlow) Begin(ctx context.Context) (Login, error) { <-ctx.Done(); return Login{}, ctx.Err() }
func (blockedFlow) Submit(string) error                      { return nil }
func (blockedFlow) Wait(context.Context) error               { return nil }
func (blockedFlow) Close()                                   {}

func TestLoginCancelWhileStarting(t *testing.T) {
	var r LoginRunner
	done := make(chan error, 1)
	go func() { _, err := r.Start(context.Background(), blockedFlow{}); done <- err }()
	for r.Status().State != "starting" {
		time.Sleep(5 * time.Millisecond)
	}
	r.Cancel()
	if err := <-done; !errors.Is(err, ErrLoginCanceled) || r.Status().State != "" {
		t.Fatalf("err = %v, status = %+v", err, r.Status())
	}
}
