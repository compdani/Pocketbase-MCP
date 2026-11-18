package chat

import (
	"errors"
	"testing"
)

func TestTurnStateTerminal(t *testing.T) {
	terminal := []TurnState{TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range []TurnState{TurnStateQueued, TurnStateRunning} {
		if s.Terminal() {
			t.Fatalf("%s should not be terminal", s)
		}
	}
}

func TestTurnStateValid(t *testing.T) {
	valid := []TurnState{TurnStateQueued, TurnStateRunning, TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted}
	for _, s := range valid {
		if !s.Valid() {
			t.Fatalf("%s should be valid", s)
		}
	}
	for _, s := range []TurnState{"", "done", "QUEUED"} {
		if s.Valid() {
			t.Fatalf("%q should be invalid", s)
		}
	}
}

func TestCanTransition(t *testing.T) {
	states := []TurnState{
		TurnStateQueued, TurnStateRunning, TurnStateCompleted,
		TurnStateFailed, TurnStateCancelled, TurnStateInterrupted,
	}
	allowed := map[TurnState]map[TurnState]bool{
		TurnStateQueued: {
			TurnStateRunning:     true,
			TurnStateCancelled:   true,
			TurnStateInterrupted: true,
		},
		TurnStateRunning: {
			TurnStateCompleted:   true,
			TurnStateFailed:      true,
			TurnStateCancelled:   true,
			TurnStateInterrupted: true,
		},
	}
	for _, from := range states {
		for _, to := range states {
			want := allowed[from][to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesAllowNothing(t *testing.T) {
	for _, from := range []TurnState{TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted} {
		for _, to := range []TurnState{TurnStateQueued, TurnStateRunning, TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted} {
			if CanTransition(from, to) {
				t.Errorf("terminal %s must not transition to %s", from, to)
			}
		}
	}
}

func TestErrInvalidTransitionIsDefined(t *testing.T) {
	if ErrInvalidTransition == nil {
		t.Fatal("ErrInvalidTransition must be defined")
	}
	if !errors.Is(ErrInvalidTransition, ErrInvalidTransition) {
		t.Fatal("ErrInvalidTransition must be errors.Is-comparable")
	}
}
