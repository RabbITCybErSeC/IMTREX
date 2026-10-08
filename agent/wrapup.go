package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// The wrap-up (settlement) prompt: when an agent is terminated because [its steps ran out (MaxTurns)] or
// [it timed out (run_seconds/MaxDuration)], the SDK's settlement phase injects this prompt so the agent
// first persists what it has identified but not yet written back, then emits a one-sentence summary, avoiding a half-finished run.
//
// Each agent's wrap-up prompt can be overridden in the admin UI as needed (stored in
// agents.wrapup_prompt); left empty it uses the builtin default here. Only [the prompt body] is editable; which tools are disabled and how many turns the wrap-up itself gets are fixed policy in code.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// The builtin default wrap-up prompts, indexed by agent key. worker reuses the historically hard-coded
// settleWrapUpPrompt (defined in worker.go), planner and mainagent each have their own; anything unmatched (a custom agent) uses the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: the builtin default turn budget for each agent's [own] wrap-up phase (overridable with >0 in the admin UI).
// All get 10 turns, which guarantees the wrap-up has enough steps to persist. Anything unmatched uses genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planning steps for this round are nearly used up -- note that this is only the end of [this round]; the system will wake you again as the state changes and you will keep planning, so the task is not ending and you need not wrap up the whole plan here. Land the conclusions you have already reached this round so it is not wasted, but **do not pad with intents just to wrap up** (0 intents this round is still a perfectly normal outcome): (1) if you have judged an exploration direction that [should be dispatched now], submit them in a single batched add_intent (do not sit on what you have decided); (2) for a goal proved achieved by some finding/fact, call prove_goal to mark it met (do not miss any); (3) if you have identified a serial exploitation chain that needs stepping through, record it with TodoWrite so the next wake-up can continue dispatching. Once done, simply end the round; no summary text is needed."

const mainAgentWrapUpDefault = "Your steps are nearly used up and this interaction is about to end. Do not start any new exploration or operation. **In a single separate plain-text sentence**, summarize the current progress, the key conclusions and the suggested next step for the user."

const genericWrapUpDefault = "You are about to be terminated because the budget is exhausted. First write back any results you have completed but not yet persisted, then **in a single separate plain-text sentence** summarize what you did and which key conclusions you reached (that sentence is shown as this run's result)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up wording (see docs/task-timeout-and-wrapup-design.md) ----------
//
// These are a [separate set] from the per-run wording: per-run means "this run's budget is spent", while
// a task timeout means "the whole task has reached its deadline and is about to end". The semantics are
// often opposite (the planner especially: per-run says "do not stop, keep planning" while a task timeout
// says "stop planning, make the final judgement"). Only worker/planner have one configured.

// WrapupTaskTimeoutOverride / ...TurnsOverride: the DB overrides for the task timeout wording and turn
// count (wired to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (this is not your run's budget, it is the entire exploration reaching its deadline). This is the last chance: (1) persist [everything] you have identified but not yet written back -- new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed findings with report_finding; (2) do not start any new command or probe; (3) **finally, in a single separate plain-text sentence**, summarize the key conclusions on your intent."

const plannerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this round, the entire task is terminating). Based on [all] the current facts and findings, make one final goal judgement: call prove_goal to mark any goal proved achieved by the evidence (do not miss any). **Do not generate any new intent** (an intent dispatched now would never run). Once judged, stop; no summary text is needed."

// TaskTimeoutWrapupDefault returns an agent's builtin default task timeout wrap-up wording (for the admin UI's placeholder / restore default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // returns an empty string when not configured (mainagent/chat)
}

// resolveTaskTimeoutWrapup: a non-empty DB override > the builtin default. An empty string means the agent has no task timeout wording
// (it is not worker/planner), and the caller should fall back to the per-run wording.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // default to the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  -> this run is squeezed by the task deadline: wrapping up because of Timeout = the task's time is up -> the task timeout wording;
//     wrapping up because of MaxTurns = the steps ran out first inside the clamped window while the task still has minutes left -> fall back to the per-run wording.
//   - clamped=false -> the task still has time: both reasons use the per-run wording (i.e. it degrades to wrapupSettlement).
//
// The harness's PromptByReason picks on the spot from the [actual] reason at wrap-up time, so there is no build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // the fallback (and the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // the task's time is up
				harness.ReasonMaxTurns: perRun, // the steps ran out first while the task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
