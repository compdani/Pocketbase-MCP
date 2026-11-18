// The Go turn reducer is a test oracle: tests replay stored event streams
// through it to check what a client would render. Browsers use the JS reducer in
// frontend/client/reducer.mjs, so this is kept out of the package's API.

package chat

import (
	"encoding/json"
	"fmt"
)

// Tool statuses in a TurnView.
const (
	ToolStatusRunning   = "running"
	ToolStatusCompleted = "completed"
	ToolStatusFailed    = "failed"
)

// ToolView is the reduced client-side state of one tool call.
type ToolView struct {
	CallID  string          `json:"callId"`
	Tool    string          `json:"tool"`
	Status  string          `json:"status"`
	Result  json.RawMessage `json:"result,omitempty"`
	Message string          `json:"message,omitempty"`
}

// TurnView is the reducer's view of one turn: exactly the state a browser
// client derives from replaying the event stream. Proposals holds one entry per
// proposal ID, in first-seen order.
type TurnView struct {
	TurnID    string         `json:"turnId"`
	State     TurnState      `json:"state"`
	Text      string         `json:"text"`
	Tools     []ToolView     `json:"tools"`
	Usage     *Usage         `json:"usage,omitempty"`
	Error     *TurnFailed    `json:"error,omitempty"`
	Proposals []ProposalView `json:"proposals"`
	LastSeq   int64          `json:"lastSeq"`
}

// Apply folds one event into the view.
//
//   - An event at or below LastSeq is a replay duplicate and is ignored.
//   - A gap (Seq greater than LastSeq+1) returns an error: part of the stream
//     was missed and the caller must reload canonical state.
//   - Any event after a terminal state returns an error, because a turn has
//     exactly one outcome.
//
// On success LastSeq advances; on error the view is unchanged.
func (v *TurnView) Apply(e Event) error {
	if v.TurnID == "" {
		v.TurnID = e.TurnID
	} else if e.TurnID != "" && e.TurnID != v.TurnID {
		return fmt.Errorf("chat: event for turn %q applied to turn %q", e.TurnID, v.TurnID)
	}
	if e.Seq <= v.LastSeq {
		return nil
	}
	if e.Seq != v.LastSeq+1 {
		return fmt.Errorf("chat: event sequence gap for turn %q: have %d, got %d", v.TurnID, v.LastSeq, e.Seq)
	}
	if v.State.Terminal() {
		return fmt.Errorf("chat: event %s after terminal state %s", e.Type, v.State)
	}

	switch e.Type {
	case EventTurnQueued:
		v.State = TurnStateQueued
	case EventTurnStarted:
		v.State = TurnStateRunning
	case EventTextDelta:
		var d TextDelta
		if err := decodeData(e, &d); err != nil {
			return err
		}
		v.Text += d.Text
	case EventToolStarted:
		var d ToolStarted
		if err := decodeData(e, &d); err != nil {
			return err
		}
		v.setTool(ToolView{CallID: d.CallID, Tool: d.Tool, Status: ToolStatusRunning})
	case EventToolCompleted:
		var d ToolCompleted
		if err := decodeData(e, &d); err != nil {
			return err
		}
		t := v.tool(d.CallID)
		t.CallID = d.CallID
		if d.Tool != "" {
			t.Tool = d.Tool
		}
		t.Status = ToolStatusCompleted
		t.Result = cloneRaw(d.Result)
		t.Message = ""
		v.setTool(t)
	case EventToolFailed:
		var d ToolFailed
		if err := decodeData(e, &d); err != nil {
			return err
		}
		t := v.tool(d.CallID)
		t.CallID = d.CallID
		if d.Tool != "" {
			t.Tool = d.Tool
		}
		t.Status = ToolStatusFailed
		t.Message = d.Message
		v.setTool(t)
	case EventUsageUpdated:
		var d Usage
		if err := decodeData(e, &d); err != nil {
			return err
		}
		u := d
		v.Usage = &u
	case EventTurnCompleted:
		v.State = TurnStateCompleted
	case EventTurnFailed:
		var d TurnFailed
		if err := decodeData(e, &d); err != nil {
			return err
		}
		f := d
		v.Error = &f
		// Restart recovery ends turns as interrupted but must still emit one of
		// the three terminal event types, so it uses turn.failed with the
		// "interrupted" code. The reducer restores the stored state from it.
		if d.Code == "interrupted" {
			v.State = TurnStateInterrupted
		} else {
			v.State = TurnStateFailed
		}
	case EventTurnCancelled:
		v.State = TurnStateCancelled
	case EventProposalCreated, EventProposalUpdated:
		var d ProposalEventData
		if err := decodeData(e, &d); err != nil {
			return err
		}
		if err := d.Proposal.Validate(); err != nil {
			return fmt.Errorf("chat: %s data: %w", e.Type, err)
		}
		v.setProposal(d.Proposal)
	default:
		return fmt.Errorf("chat: unknown event type %q", string(e.Type))
	}

	v.LastSeq = e.Seq
	return nil
}

func (v *TurnView) tool(callID string) ToolView {
	for _, t := range v.Tools {
		if t.CallID == callID {
			return t
		}
	}
	return ToolView{CallID: callID, Status: ToolStatusRunning}
}

func (v *TurnView) setTool(t ToolView) {
	for i := range v.Tools {
		if v.Tools[i].CallID == t.CallID {
			v.Tools[i] = t
			return
		}
	}
	v.Tools = append(v.Tools, t)
}

// setProposal inserts or replaces one proposal by ID. A later frame always
// carries a higher-or-equal revision, because every stored mutation increments
// it; an older frame that arrives out of order therefore cannot undo a newer
// state. A replayed frame with the same revision is harmless, and no frame
// ever duplicates an entry.
func (v *TurnView) setProposal(p ProposalView) {
	for i := range v.Proposals {
		if v.Proposals[i].ID != p.ID {
			continue
		}
		if p.Revision >= v.Proposals[i].Revision {
			v.Proposals[i] = p
		}
		return
	}
	v.Proposals = append(v.Proposals, p)
}

func decodeData(e Event, out any) error {
	if len(e.Data) == 0 {
		return fmt.Errorf("chat: %s event without data", e.Type)
	}
	if err := json.Unmarshal(e.Data, out); err != nil {
		return fmt.Errorf("chat: decode %s data: %w", e.Type, err)
	}
	return nil
}
