package engine

import (
	"errors"
	"testing"

	"recogn/internal/onnxrt"
)

// TestGateSlotReleasedOnPanic pins the L7 fix (SECURITY-REVIEW.md): a panic
// inside a model Run must not leak its concurrency-gate slot. net/http
// recovers handler panics, so a leaked slot would never be reclaimed — once
// every slot had leaked, every inference call would block forever.
func TestGateSlotReleasedOnPanic(t *testing.T) {
	c := &cgoInferencer{slots: make(chan struct{}, 2)}
	sentinel := errors.New("run panicked")

	func() {
		defer func() { _ = recover() }() // the same recovery net/http applies to handlers
		_, _ = c.gatedRun(func() ([]onnxrt.Tensor, error) { panic(sentinel) })
	}()

	if got := cap(c.slots) - len(c.slots); got != cap(c.slots) {
		t.Fatalf("slot leaked on panic: only %d of %d slots available", got, cap(c.slots))
	}

	// The gate must still admit and release a normal run afterwards.
	if _, err := c.gatedRun(func() ([]onnxrt.Tensor, error) { return nil, nil }); err != nil {
		t.Fatalf("gated run after panic: %v", err)
	}
	if got := cap(c.slots) - len(c.slots); got != cap(c.slots) {
		t.Fatalf("slot leaked on normal run: only %d of %d slots available", got, cap(c.slots))
	}
}
