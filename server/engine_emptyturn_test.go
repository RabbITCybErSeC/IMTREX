package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// Recognizing and continuing an empty turn (thinking only, no text and no tool); see steerHooks.Stop.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "scan the ports first"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"thinking only", []llm.Message{llm.UserText("start"), assistantThinking("let me think")}, true},
		{"thinking + tool", []llm.Message{llm.UserText("start"), toolUse}, false},
		{"thinking + text", []llm.Message{assistantThinking("let me think"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("conclusion")},
		}}, false},
		{"the text is whitespace only", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"a completely empty assistant turn", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// A tool result carries the user role, so the decision must look back to the assistant message before it instead of misjudging the nearest one.
		{"the last message is a tool result", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"no assistant message at all", []llm.Message{llm.UserText("start")}, false},
		{"empty history", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks is a programmable inner HookRunner, used to verify that steerHooks respects the inner decision.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("I should enumerate the subdomains first")}

	t.Run("an empty turn gets a continue instruction injected", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("an empty turn should not be hard-stopped")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("no intervention when there is text or a tool", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("the scan is done, no open port was found")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("a normal wrap-up was misjudged as an empty turn: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("nothing should be counted when there is no intervention, got %d", n)
		}
	})

	t.Run("let the turn end once the limit is reached", func(t *testing.T) {
		const limit = 5 // the user set "empty response retries" to 5
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("attempt %d should still be within the budget, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("still injecting beyond the limit: %v", blocking)
		}
	})

	// Setting "empty response retries" to -1 turns this layer off, and emptyTurnNudgeLimit parses to 0.
	t.Run("no intervention when it is configured off", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("still injecting after being turned off: %v", blocking)
		}
	})

	t.Run("no stacking when inner decides to hard stop", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "the guard refused to let the turn end"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "the guard refused to let the turn end" || blocking != nil {
			t.Fatalf("inner's hard stop was overwritten: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("no budget should be consumed when yielding to inner, got %d", n)
		}
	})

	t.Run("no stacking when inner already asked to continue", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"the guard's reason for continuing"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "the guard's reason for continuing" {
			t.Fatalf("inner's continue message was overwritten: %v", blocking)
		}
	})

	t.Run("the behaviour is unchanged when no counter is installed", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // e.g. some future call site forgets to pass nudges
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("nothing should be injected without a counter: %v", blocking)
		}
	})
}
