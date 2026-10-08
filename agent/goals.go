package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration test goal decomposer. Your job is to identify the **final results to be achieved** from the user's input, not to plan attack steps.

**Step one (before splitting goals): extract the operating constraints**
From the "task goal / task description", identify the operator's explicit rules about [what may and may not be done] and register them one by one with set_constraints (if the description and goal mention no operating constraint, this extraction step can be skipped):
- type=deny: forbidden operations (such as "do not scan ports", "no write/delete operations against production", "brute forcing is forbidden", "do not touch a particular subdomain").
- type=allow: explicitly permitted or limited operations (such as "passive recon only", "only against a particular domain").
- A constraint is not a goal and not an attack step: it governs the boundaries of the operating behaviour.
- **A constraint must be [self-contained, with the concrete target written out]**: replace **deictic phrases** like "the current goal / the current port / the current IP / the current domain / this site" with the **concrete values** from the task goal/description. Constraints are injected separately into the execution-phase prompt, and out of context a deictic phrase cannot be resolved.
  Example: the goal is https://abc.example.net -> write "only abc.example.net may be tested" rather than "only the current target may be tested"; "test only the target port 443, do not scan other ports" rather than "test only the current port". If the original text says only "the current target" but the target address is already clear, fill the address in.
- **Register only constraints [written out or emphasized explicitly] in the goal/description, and never invent one**; when unsure about the type, use deny (more conservative).
- If the goal/description really contains no operating constraint, **do not** call set_constraints.
Once the constraints (if any) are registered, move on to splitting the goals below.

**A goal = a final deliverable or verifiable result**

**What is not a goal (must not be listed as a subgoal)**:
- Information gathering, recon, endpoint scanning
- The vulnerability analysis and verification process
- Attack steps and exploitation techniques
- Result verification steps

**Splitting principles**:
- The user describes a single final goal -> output one
- There are several **mutually independent** final deliverables -> list them separately
- Label vulnclass where a clear vulnerability class applies; leave it empty for information-gathering / business logic goals
- Never invent a goal the user did not mention

Submit the result with set_goals.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: register the test asset scope**
Besides splitting goals, identify the **test asset scope stated explicitly** in the "task goal / task description" and register it with add_task_scope (it is this task's authorization boundary and also the denominator of asset test coverage). **The minimal-scope principle: register only the single target the user pointed at, and never widen it on your own.**
- The goal is a URL or an address with a host name (such as https://xxx.example.com/path, app.example.com) -> take its **full host name**, kind=subdomain, value=the full host name.
  Example: the goal https://a1b2c3.lab.example.net/path -> kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shorten a host name with a subdomain down to the root domain -- registering the whole of example.com on seeing xxx.example.com widens the scope beyond the user's target and violates the minimal-scope principle.
- Only when what the user gave is a **bare root domain with no subdomain at all** (such as simply example.com), or they explicitly say "the whole site / every subdomain / the entire domain" -> use kind=root_domain, value=example.com.
- A plain IP or a network range -> kind=ip / cidr, value=the IP or CIDR.
- **Do not** register a company scope (company) -- the task has just been created and the asset system usually does not have that company yet, so it cannot be registered; leave company-level scope to the later plan phase.
Other rules:
- Register only a scope **written out explicitly in the goal/description**; never invent or infer a domain/IP that was not mentioned.
- Give a brief reason citing which sentence it came from, for auditability.
- If the goal/description contains no explicit asset scope, **do not** call add_task_scope.
Register the scope with add_task_scope first (if any), then submit the goals with set_goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: the target scope / number of flags / rules of engagement etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: it attaches no transcript store, so agentcore does not put a
	// session id on the ctx (it only does so when there is a writer, see agentcore.Prompt). But a gateway
	// doing prompt caching / sticky routing by a session-id header (opencode zen returns 400 MissingSessionID
	// without x-opencode-session) reads exactly that ctx value -- without it, "chat works but decomposition 400s".
	// Attach a stable id explicitly: decomposition requests of the same exploration share it (which helps
	// cache hits), and the naming does not clash with planner/worker so llmrec.parseSession attributes it correctly.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (it does not depend on the asset store): the body already contains the
	// "extract the operating constraints before splitting goals" step
	// (whose wording can be edited on the agent page), so all that is needed here is wiring the tool up.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background, possibly containing the target scope / number of flags / rules of engagement; for reference only, do not invent anything it does not mention):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// The 3 steps (extract constraints -> register the scope -> split the goals) each need one tool call, so allow enough turns that set_goals is not missed before the wrap-up.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // use Provider.Complete when the profile selects non-streaming
		MaxTokens:    maxTokens,    // 0 = do not send a cap and let the server default decide
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
