package chat

import "errors"

// TurnState is the lifecycle state of a turn.
type TurnState string

const (
	TurnStateQueued      TurnState = "queued"
	TurnStateRunning     TurnState = "running"
	TurnStateCompleted   TurnState = "completed"
	TurnStateFailed      TurnState = "failed"
	TurnStateCancelled   TurnState = "cancelled"
	TurnStateInterrupted TurnState = "interrupted"
)

// ErrInvalidTransition is returned when a state change is not part of the
// lifecycle, for example completed -> running.
var ErrInvalidTransition = errors.New("chat: invalid turn state transition")

// transitions is the complete set of edges the state machine allows. A turn
// has exactly one terminal state and never leaves it.
var transitions = map[TurnState][]TurnState{
	TurnStateQueued:  {TurnStateRunning, TurnStateCancelled, TurnStateInterrupted},
	TurnStateRunning: {TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted},
}

// Valid reports whether s names a known state.
func (s TurnState) Valid() bool {
	switch s {
	case TurnStateQueued, TurnStateRunning, TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted:
		return true
	}
	return false
}

// Terminal reports whether s ends the turn.
func (s TurnState) Terminal() bool {
	switch s {
	case TurnStateCompleted, TurnStateFailed, TurnStateCancelled, TurnStateInterrupted:
		return true
	}
	return false
}

// CanTransition reports whether the state machine allows from -> to.
func CanTransition(from, to TurnState) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// terminalEventFor reports the one terminal event type that ends a turn in
// state s. ok is false for non-terminal states.
func terminalEventFor(s TurnState) (EventType, bool) {
	switch s {
	case TurnStateCompleted:
		return EventTurnCompleted, true
	case TurnStateCancelled:
		return EventTurnCancelled, true
	case TurnStateFailed, TurnStateInterrupted:
		return EventTurnFailed, true
	}
	return "", false
}
