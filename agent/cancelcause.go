package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "the user paused the task",
		"The user paused the task through the task control endpoint (POST /api/tasks/{id}/control, action=pause). This planner/worker run was cancelled deliberately; a running intent returns to the frontier (open) and is reclaimed and run from the start once the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "the orchestration agent paused the task",
		"The orchestration agent called the pause_task tool to pause this task. This planner/worker run was cancelled deliberately; a running intent returns to the frontier (open) and runs again after it resumes")
	AbortTaskDeleted = cause("task_deleted", "the task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}), and the deletion barrier cancelled the planner, workers and main agent running for it; this run's result will not be used")
	AbortPausedOnReload = cause("paused_on_reload", "backend restored the paused state",
		"On startup the backend restored the task's paused state from what was persisted in the database. This run was cancelled; normally no agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "planner judged the goals achieved",
		"The planner judged the task's goals achieved and set the task to done, then cancelled the workers still running; those intents are marked stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "task timeout wrap-up wait ran out",
		"After the task reached its timeout it waited for running workers to wrap up gracefully, but the 90-second drain grace period was still not enough, so a hard cancellation was performed; the intents are marked exhausted and the facts and assets already written during the wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "the planner terminated this intent",
		"The planner called kill_work to terminate this intent deliberately, usually meaning the direction went off track or is no longer worth continuing; the intent is marked stopped and is not reclaimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "the user paused this worker intent",
		"The user paused a running worker. This call was cancelled and the intent became paused; the intents, facts, findings and activity records already registered are all kept, and it runs from the start again after resuming")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "the user deleted this worker intent",
		"The user deleted a running worker. This call was cancelled; once the worker leaves the write region, the server handles the intent per the deletion mode the user chose -- a soft delete only marks it deleted and keeps every product, while a hard delete cascades and removes the intent and the downstream nodes supported only by it")
	AbortWorkFinished = cause("work_finished", "worker finished and released context",
		"The worker finished normally and the engine released its context resources in detachWork. This is not an interruption; if it appears in an interruption message, cancellation and the finish event raced")
	AbortPausedRaceGuard = cause("paused_race_guard", "new run refused while task paused",
		"While the task is paused the engine refuses to issue a new execution context, which prevents a race between claiming and pausing from letting a worker start anyway; an already claimed intent returns to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "the user stopped this conversation turn",
		"The user clicked stop, deliberately aborting this main agent or session agent run. The activity records already produced are kept and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "task paused, main agent chat aborted",
		"When the user paused the task, the main agent conversation running at the time was cancelled with it. The activity records already produced are kept; this turn's message is not replayed automatically when the task resumes")
	AbortChatTurnFinished = cause("chat_turn_finished", "chat turn finished, context released",
		"This conversation turn finished normally and the server is releasing the turn's context resources. This is not an interruption; if it appears in an interruption message, cancellation and the finish event raced")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "the backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating or shutting down. Every running agent is cancelled; after a restart, leftover running intents are reset to open and run again")
	AbortRunHardTimeout = cause("run_hard_timeout", "per-run hard-timeout backstop fired",
		"A single run exceeded its soft wall-clock budget plus the extra grace period, meaning a model request or some tool had not returned for a long time and the normal turn-boundary wrap-up could not run. Focus on the last tool call that never returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "upstream context reached its deadline",
			"The upstream context reached its deadline, but whoever set it attached no named cause through WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "the canceller attached no named cause",
			"The upstream context was cancelled, but the canceller attached no named cause through context.WithCancelCause; register the cause in agent/cancelcause.go and wire it into that cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
