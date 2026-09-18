package agent

import (
	"context"
	"testing"
)

func TestActiveTurnOldGenerationCannotDeleteReplacement(t *testing.T) {
	l := &Loop{active: map[string]activeTurn{}}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	genA := l.registerActive("session", cancelA)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	genB := l.registerActive("session", cancelB)

	select {
	case <-ctxA.Done():
		// expected: replacement cancels A
	default:
		t.Fatal("replacement did not cancel previous generation")
	}

	l.unregisterActive("session", genA)
	current, ok := l.active["session"]
	if !ok {
		t.Fatal("old generation removed replacement")
	}
	if current.generation != genB {
		t.Fatalf("active generation=%d want %d", current.generation, genB)
	}

	if !l.cancelActive("session") {
		t.Fatal("cancelActive should find replacement")
	}
	select {
	case <-ctxB.Done():
		// expected
	default:
		t.Fatal("cancelActive did not cancel replacement")
	}
}

func TestTryRegisterActiveRejectsDuplicateWithoutCancellation(t *testing.T) {
	l := &Loop{active: map[string]activeTurn{}}
	firstCanceled := false
	if _, ok := l.tryRegisterActive("session", func() { firstCanceled = true }); !ok {
		t.Fatal("first turn was rejected")
	}
	if _, ok := l.tryRegisterActive("session", func() {}); ok {
		t.Fatal("duplicate turn was accepted")
	}
	if firstCanceled {
		t.Fatal("duplicate turn canceled the in-flight provider")
	}
}
