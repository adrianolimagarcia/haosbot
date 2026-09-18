package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
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

	// Cancellation is only a signal. The generation must remain registered
	// until the canceled turn's deferred unregister runs, otherwise a new turn
	// can overlap the old turn while it is still unwinding.
	if _, ok := l.tryRegisterActive("session", func() {}); ok {
		t.Fatal("new turn was accepted before canceled generation unwound")
	}
	l.unregisterActive("session", genB)
	genC, ok := l.tryRegisterActive("session", func() {})
	if !ok {
		t.Fatal("new turn was rejected after canceled generation unregistered")
	}
	l.unregisterActive("session", genC)
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

func TestMutatingCommandsRejectWhileTurnActive(t *testing.T) {
	for _, command := range []string{"/new", "/compact"} {
		t.Run(command, func(t *testing.T) {
			store := newFakeStore()
			l := newTestLoop(t, store, nil)
			msg := core.InboundMessage{Channel: "cli", ChatID: "same-session", Content: command}
			key := msg.SessionKey()

			activeCanceled := false
			gen, ok := l.tryRegisterActive(key, func() { activeCanceled = true })
			if !ok {
				t.Fatal("could not seed active turn")
			}
			defer l.unregisterActive(key, gen)

			out, err := l.ProcessMessage(context.Background(), msg)
			if !errors.Is(err, ErrTurnActive) {
				t.Fatalf("%s error = %v, want ErrTurnActive", command, err)
			}
			if out != nil {
				t.Fatalf("%s returned output while another turn was active: %#v", command, out)
			}
			if activeCanceled {
				t.Fatalf("%s canceled the existing turn instead of waiting for exclusivity", command)
			}
			if got := store.get(key); got != nil {
				t.Fatalf("%s opened or mutated transcript despite active turn: %#v", command, got)
			}
		})
	}
}
