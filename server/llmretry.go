package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side resolution of the retry policy, see docs/llm-retry-design.md. Of the five layers:
//   - connect / empty response / same-provider safe window "travel with the endpoint", so each LLM configuration can override
//     the global default (leaving a profile field empty inherits the global one, and with no global value the builtin default applies);
//   - the circuit breaker / intent rerun are process-level and exist only globally.
//
// The global policy reads one settings row from the DB, and every call site is on a low-frequency path (building a provider, a work wrap-up,
// saving a configuration), so another cache layer is not worth it; the circuit-breaker parameters are the exception -- they are read on
// every failure, so applyRetryPolicy pushes them to the Registry to hold.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// The counts keep their original "0 = default / negative = off" semantics here: the SDK's MaxRetries /
		// EmptyResponseRetries are structurally identical, so it can resolve them itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// The circuit-breaker (failover cooldown) defaults, matching llmpool's builtin ones -- this only overrides them when the user configured a value.
// The intent-rerun defaults are modelErrorRetries / modelErrorRetryBackoff in engine.go.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer (2)'s knob -- the "empty response
// retry count": the two are two means to the same end. The SDK layer handles "not a single content block" by resending
// the identical request; this one handles "thinking only, with neither body nor tool" by appending an instruction so the
// model continues from the thinking it already has (resending verbatim is pointless for an empty turn determined by the shape of the context). The emptiness tests
// differ because the SDK goes by "was any event yielded" and a thinking delta is itself an event -- but when a user configures
// "how many empty-response retries" what they mean is "the model produced nothing substantive, so try again", and sharing one count across both layers
// matches that mental model.
//
// It reads the global policy rather than a profile's override: a run may switch profile mid-way through failover, while this
// is a total gate for the whole intent and should not change with the endpoint. The semantics mirror the SDK's emptyRetries():
// 0 = the default defaultEmptyTurnNudges; -1 (negative) = disable empty-turn continuation; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
