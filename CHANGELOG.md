# Changelog

Notable changes to this project are recorded in this file, following the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### Added

- **New built-in intercept rule "delete-style endpoint paths"**: the built-in HTTP destructive rule previously only recognized the DELETE **method**, while most applications trigger deletion through GET/POST — a call like `curl 'http://t/api/user/delete?id=1'` would really delete target data. A new `deny` rule now covers `/delete /del /remove /unlink /erase /destroy` (a separator must follow the verb, so `/delivery`, `/details` and `/delta` are not caught by mistake). Existing instances get it on upgrade and can disable or delete it under "System → Command interception".
- **Model-fallback approval token usage is now visible on its own**: approval calls previously had no separate metering, so spend could only be guessed. They are now metered separately, and a usage card appears under the "Model fallback approval" toggle in "System → Command interception" — cumulative calls, input, output, cache read/write, plus a mini bar chart of the last 30 days.

### Finding notifications

#### Added

- **Findings can be pushed to IM**: a new "System → Notifications" page pushes scan findings to six channel kinds — **DingTalk, Feishu, WeCom, generic Webhook, Telegram and email**. Any number of instances per kind, each with its own on/off switch, rate limit and filter rules.
- **Two delivery modes: realtime and digest**: realtime sends each match immediately; digest merges them into one message per global period (default 30 minutes) headed by the new count and severity distribution. Want "high severity realtime, everything else digested"? Create two channels with different settings.
- **Filters cover four dimensions**: minimum severity, task / asset scope, vulnerability-class keyword include and exclude (exclude wins), and whether to receive disposition status changes (off by default). Messages carry a "View details" button whose target comes from the global "link base URL" setting; empty means no button.
- **Delivery history and manual resend**: lists every delivery's state, attempt count, failure reason and channel, filterable by channel and state; failed items can be resent with one click (resending resets the retry count).
- **Channel credentials echo back masked**: webhook URLs, signing secrets, bot tokens, SMTP passwords and the like are echoed only as masked values with a tail hint; sending them back unchanged means "keep", clearing the field deletes it.

#### Fixed

> This section records a security audit after the feature's first version: every item was reproduced, fixed and covered by a regression test.

- **Fixed mask bypass via "change the target, keep the credentials" (severe)**: config merging kept the stored value for any key not mentioned, so changing only the address while staying silent about credentials made the server send the real stored credentials to an arbitrary address, completely silently. A change of target address now requires every credential field to state itself explicitly — a new value, or an explicit empty string meaning it is no longer needed.
- **Fixed zero escaping of untrusted content in markdown-style channels**: finding titles and summaries come from model output and asset URLs from scan results; a crafted title rendered as a clickable external link in DingTalk / WeCom / Feishu, and image syntax would be fetched by the client on render, leaking the reader's IP. Now normalized to a single line + markdown metacharacter escaping, applied at each channel's render exit rather than in a shared title helper.
- **Fixed the SSRF surface on delivery addresses**: loopback, internal and cloud metadata addresses were all deliverable, and the first 200 bytes of a failed response body went into `last_error` and were echoed by the delivery history — a semi-blind read primitive. The guard now sits at the **dial stage**, covers DNS rebinding and rejects cross-host redirects; loopback and link-local addresses require an explicit `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private ranges remain allowed (self-hosted internal SMTP relays are common).
- **Fixed notification credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key` and Telegram's bot token all live in the URL, and on delivery failure the full URL flowed into `last_error`, the delivery history, server logs and frontend toasts. All are now redacted down to `scheme://host` plus the underlying reason.
- **Fixed silent loss when a truncated digest batch was marked delivered wholesale**: truncated findings appeared neither in the message nor in the failure list, while the delivery history said success. Messages are now packed **per whole item**; only those included in this message are marked, the rest return to the queue for the next message without consuming retries, and the header states how many will continue in the next one.
- **Fixed a typo in the minimum-severity threshold silently disabling the filter**: a typo like `hgih` for `min_severity` gave the unknown level an ordinal of 0, degenerating the check into always-true, so users thought they were receiving only high severity while every finding was poured into the group. The write path now validates the value and lists the options.
- **Fixed `rate_per_min = 0` (unlimited) being unreachable**: the write layer quietly turned an explicit 0 into the default, so operators thought they had lifted the limit while it still applied. Defaults now move to the API layer and are filled only when the field is absent.
- **Fixed digest channels bypassing the token bucket entirely**: `rate_per_min` previously had no effect in digest mode. The number claimed per round is now bounded by both the round budget and an in-memory upper bound.
- **Fixed failure handling using the batch's max attempt count, letting old deliveries drag down new ones**: an old delivery that had retried twice would pull brand-new deliveries in the same batch into failed, permanently losing new findings that had not used a single retry. Decisions are now per item.
- **Fixed deliveries whose snapshot cannot be parsed being silently marked successful in a digest batch**: such entries were skipped at render time yet counted as successes with the batch. They are now explicitly failed with a reason.
- **Fixed a single channel's per-round claim count possibly exceeding the lease duration**: in multi-instance deployments, rows with expired leases were claimed again, sent twice and double-counted attempts. The per-round cap is now derived backwards from "lease / single attempt timeout", with an assertion pinning the relationship between these constants.
- **Fixed Telegram truncation possibly cutting HTML entities**: truncation avoided half tags but not entity fragments like `&amp`, and the parser could then reject the **entire** message. Both are now avoided.
- **Fixed the email channel treating transient SMTP failures as permanent**: greylisting 4xx codes like `450` are temporary rejections that should be retried later, but were judged fatal, so servers with greylisting would fail every push on its first attempt. The first digit of the response code now decides: 4xx retryable, 5xx permanent.
- **Fixed nested-structure submissions actually writing the mask literal into the database**: object fields like `webhook.headers` can only be masked or submitted as a whole; stuffing the mask sentinel inside the object stored it as a real value and silently broke authentication. Such submissions are now explicitly rejected.

#### Design notes

- **The finding-write transaction does a single blind INSERT**: `notification_events` is written in the same transaction as the finding, so commit guarantees both are atomically consistent. This INSERT deliberately does not read the channel table or run user filter rules, and is wrapped in a `SAVEPOINT` — otherwise one misconfigured filter could abort the transaction and keep a high-severity finding out of the database.
- **Delivery claims use leases, not long transactions**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as the lease; no database lock is held during delivery. Rows left behind by a crashed process are re-claimed after the lease expires, self-healing without infinite retries.
- **Rate limiting does not consume the retry budget**: the round budget is computed from the token bucket first, then that many rows are claimed. The reverse order would waste an attempt on a rate-limited delivery, burning all three attempts on pure waiting before failing.

### Test infrastructure

#### Fixed

- **Fixed permanent asset leftovers caused by cleanup ordering in the company-ICP attribution test**: `defer d.Close()` ran first on function return and `t.Cleanup` after it, so every cleanup statement hit an already-closed connection, errors were discarded, and test assets and companies stayed in the database forever — also making other asset-count assertions fail mysteriously. Closing the connection now goes through `t.Cleanup` registered first, and cleanup failures are surfaced.

### Traffic

#### Added

- **Traffic list gained content search, path / status / length filters and column sorting**: the filter row supports **response content** (via the full-text index), **path**, **status code** (exact or buckets like `2xx`) and **response-length range**, and headers sort by time, status code or response length with the preference stored locally. Two new indexes apply automatically on upgrade; the agent tool's query contract is unchanged.
- **New "Clear all" in the traffic list**: deletes every traffic record at once (ignoring current filters), cleans up historical host directories the index no longer tracks, and follows up with a full compaction reporting how much space was actually released. Traffic evidence already bound to findings lives in a separate evidence store and is unaffected.

#### Fixed

- **Fixed disk space not being released after deleting traffic**: the index database was created without `auto_vacuum`, so the file never shrank; the full-text index also used `contentless_delete`, where `DELETE` only writes tombstones without reclaiming the original postings — deleting traffic made the index grow (measured: 6MB of captured bodies → 16MB index, still 16MB after deleting everything). New index databases enable `auto_vacuum=incremental`, and each delete runs a background chunked full-text merge plus incremental reclaim, gradually returning space to the system (the same scenario drops to 104KB); reclaim releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set at database creation, so **existing instances' index databases stay in the old mode** where incremental reclaim is a no-op (a notice is printed at startup). On such databases the tombstone growth is already stopped; reclaim the occupied size once with "Clear all" — that step converts the database to incremental-reclaim mode, after which day-to-day deletes take effect by themselves.

### LLM

#### Fixed

- **Fixed the custom session header not taking effect on "one-shot LLM calls"**: goal decomposition (round 0) and cold-node compaction are one-shot calls without a transcript store, so the context had no session id and the gateway could not read the header — against endpoints like opencode zen that return 400 when it is missing, the symptom was "round 0 fails while later planner rounds work perfectly". Both paths now attach an exploration-stable session id, which also lets token usage be attributed to the right exploration (previously not recorded at all).

### Tools

#### Fixed

- **Fixed output truncation not taking effect for MCP and custom tools, letting large results flood the context**: MCP tool returns were not truncated at all, and HTTP custom tools used a hardcoded 6000 characters instead of the session limit because they received an empty tool context. Both paths now go through `actool.Capture`: truncate by the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is set, spill the full output to disk keeping only the leading excerpt and a file pointer in the message.
- **Upgraded norma to v0.4.3**: the harness layer uniformly runs `CaptureOnce`, so no tool (including future ones) can overflow the context by missing truncation.

### Deployment

#### Fixed

- **Fixed the activity stream being unreachable behind a reverse proxy**: the SSE URL was unconditionally built as `${hostname}:8787`; in a deployment proxied to 443 the page loaded from `https://domain/` while SSE pointed at port 8787, which is not exposed publicly, showing up as the activity stream reconnecting endlessly. It now follows `NODE_ENV`: same-origin by default in production, with `:8787` kept only under `next dev` to bypass Next.js buffering of SSE; the `NEXT_PUBLIC_SSE_BASE` override remains. The README gained reverse-proxy deployment notes.

### Findings

#### Fixed

- **Fixed the missing scrollbar when the asset list overflows in the "by asset" view**: the outer card only had a `max-height` without a definite height, so the scroll viewport's percentage height could not resolve and a long asset list either burst the card or was cut off without scrolling. The height cap now sits on the asset tree's native scroll container and adapts to the window height.

### Database

#### Fixed

- **Capped the connection pool**: `database/sql` by default does not limit how many connections the application itself opens; with no idle connection available it would unconditionally create new ones until PostgreSQL's `max_connections` (default 100) rejected them. The cap is now 32, with excess queries waiting for an idle connection — the same load now shows up as slower rather than erroring; if `max_connections` was lowered, lower this in step.
- **Improved connection reuse**: `MaxIdleConns` defaults to 2, so connections beyond two concurrent users were closed on return and the next request had to redo the TCP and PostgreSQL authentication handshake. The idle cap now matches the total cap, plus a 30-minute connection lifetime and 5-minute idle eviction so the pool can naturally rotate out long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **New "Findings" column in the task list**: shows counts bucketed by severity — critical / high / medium / low — with non-zero buckets colored by severity, making each task's finding scale and distribution visible at a glance.

#### Changed

- **"Description / goal" columns narrowed**: overlong content is ellipsized with the full text on hover, so long text takes less horizontal space in the list.

### Exploration graph and planning-situation overview

#### Changed

- **Slimmed down the planning-situation overview (graph_overview) and capped every list**: recent facts, finished intents, pending intents, cold-zone summaries and confirmed-finding details now use "latest window + count fallback", with anything not shown available on demand; this significantly reduces per-round LLM context size and prevents context bloat on long tasks (the cold zone of the related-task overview is throttled the same way).

#### Fixed

- **Eliminated the duplicated reverse edges between digest and finding in the exploration graph after collapsing cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset interception

#### Added

- **New global asset intercept rules (blocklist)**: accepts exact and fuzzy domain / IP / URL matches plus CIDR ranges, with full CRUD and enable/disable, managed under "System → Asset interception"; ships with built-in fuzzy blocking of government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites.
- **Asset interception wired into the execution chain**: before dispatching an intent (`add_intent`) or inserting assets (`insert_assets`), the agent checks the target assets — intents that hit interception are not dispatched and intercepted assets are not inserted, with the asset info and intercept reason returned to the agent.
- **New task-level intercept / allow (allowlist) rules**: independent of global rules and effective only for that task, evaluated as "intercept first, then allow" — an intercept hit forbids testing; if no intercept hit but the task has allow rules and none of them match, testing is "not allowed"; with no allow rules configured the allowlist is inactive. Entered when creating a task, or added/edited/enabled/disabled from the task detail "Overview".
- **Task templates can preset a category and task-level intercept/allow rules**: a template stores a task category and a set of task-level rules, carried into the new-task form when applied.

### Operation review

#### Fixed

- **Tightened the model-judge output protocol to reduce fail-open from truncation**: the verdict comment limit drops from 500 to 120 characters and the model must "output JSON only, no preamble or code fences", preventing a verdict truncated by `MaxTokens` from being unparsable and then allowed through by the model-failure policy (fail-open).

### Task archive

#### Fixed

- **Archiving skips symlinks instead of failing the whole package**: the archive format only supports regular files and directories end to end, so any symlink in the working directory previously failed the entire task archive; symlinks are now skipped and logged while the remaining files archive normally (links are not followed and never escape the directory tree).

### Account and compliance

#### Added

- **New "Usage notice and disclaimer" dialog before login**: agreement must be checked before signing in.

### Licensing and dependencies

#### Changed

- **The project is now licensed under AGPL-3.0**, with the README's licensing and disclaimer sections updated.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration-trail bulletin board

#### Added

- **New "exploration-trail bulletin board" on the task detail page** (#144): presents the task's exploration nodes (start / goal / intent / fact / finding / hint / digest) as a chronological feed, with type filters, keyword search, ascending/descending order, pagination and auto-refresh; while on "page one, newest first" it is a live position refreshing each round, and when you leave it only the unread count accumulates without disturbing your reading, with a "N new bulletins · back to latest" one-click return. Grouped by day, so even on long tasks paging back still tells you "which day this was".
- **Each bulletin row shows the node id** (#145), making it easy to cross-reference the exploration graph and locate a specific node.
- **Bulletin node detail expands to show upstream/downstream and anchored assets** (#147): expanding a bulletin shows the node's upstream (where it came from) / downstream (what it produced) relations, and hovering a related entry pops up that node's card (type/state/origin/time/summary/payload excerpt); it also lists the assets anchored to the node (type label + recognizable text). The data ships with the bulletin page, so expanding sends no extra request.
- **Bulletin search supports filtering by node id** (#150): beyond "content / origin", the search box now matches node ids — type a plain number or the displayed "#41" form to pinpoint the node.

### Intent management

#### Added

- **Intent deletion supports "soft delete / hard delete"** (#149): pending, running and paused intents can all be deleted, with a reason required; the mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is marked "deleted" with the reason in a dedicated field, keeping the intent node and all of its output and lineage.
  - **Hard delete**: physically removes the intent plus the exclusive descendant nodes "supported only by it" (cascading along the yields / intent chain to the leaves) to avoid orphaned data; the deleted intent's token metering is archived under its original dates; shared nodes (still referenced by other intents), goals and the task's root fact are always kept, and the hard-delete dialog shows the estimated cascade size.

  Both modes notify the planner "this intent was deleted by the user + reason" and re-plan accordingly.

### Operation review

#### Added

- **Approval records can be filtered by state and decision source** (#139): filter approval records by approval state and decision source to find the target record quickly.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by the history list's pagination and stay fully visible while paging through history.

### Agent

#### Changed

- **Worker role description reworded as a general cybersecurity platform** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: the per-task planner now has the Compactor wired in; cold-node compaction (cold-digest) previously never ran because it was not connected, and now works.

### Chat

#### Added

- **Chat supports @ references to multiple record types and scroll pagination** (#135): @-reference several record types in chat, with the candidate list supporting scroll pagination.

#### Fixed

- **Long message bubbles stay inside the conversation panel** (#137): overlong message bubbles no longer overflow the conversation area.
- **Fixed "missing upload file" when uploading before a new conversation exists**: the Composer's file picker snapshots the `FileList` into an array before clearing the input and then calls back; previously the draft state required asynchronously creating the conversation first, and by the time execution resumed the `FileList` — still live-bound to the input — had been cleared, so the upload lacked the file field and the backend returned 400.

### Traffic

#### Fixed

- **The traffic-recording proxy now listens on 127.0.0.1 only by default** (#129, #130): prevents the open proxy from being exposed on other network interfaces under the default configuration.

### Web

#### Fixed

- **Fixed the global header missing on the task list page in static-export builds**.
- **Added the missing mock for linked traffic on the demo finding detail page, fixing the blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### Added

- **Approval records support source navigation** (#125): clicking an approval record's "source" jumps back to the tool execution that triggered it — opening the original full conversation, auto-paging to the target position, expanding the command and result in place with a centered highlight in the scroll area while surrounding messages stay readable; auto-correction stops when the user scrolls manually. Across ordinary chats, task workers, the planner and main-agent segmented sessions, it pinpoints via the persisted `tool_use_id` plus a task mapping (with a new scoped call-id index), showing an explicit notice when it is missing, duplicated or ambiguous instead of jumping to a different execution. Clear notices are given when the conversation was deleted or the task archive/record is missing.
- **Approval records support paging** (#115).

#### Changed

- **Slimmed the model-review input** (#124, #125): the model review (the LLM judge after no rule matches) input converges to versioned JSON of "this one full tool call + an explicitly chosen short background + the local working directory". The background takes only real user messages (the current user message of a chat/task main agent); workers no longer carry an intent summary and have inherited background from a parent agent cleared; the planner and auto-triggered sessions do not fabricate user messages. It no longer carries the task description, goals, operation constraints, global exploration situation, historical calls or the worker's full intent — those still drive agent execution and standalone session audits as before, they simply do not enter operation review. Every verdict must output JSON `decision`/`comment`, and the comment must cover "the actual operation, the consequence if it succeeds, the rule that matched"; instructions inside parameters or background cannot change the review policy. The exact input snapshot sent to the model is saved with a digest, old snapshots are kept and labeled with their version, and old inputs are never reconstructed from current data.

#### Fixed

- **A model verdict wrapped in a code block is no longer silently allowed** (#126): when the model's verdict JSON was wrapped in ``` fences, parsing failed and the model-failure policy allowed it silently; the code fences are now stripped before parsing, and only a still-failing parse goes through the configured failure policy.

### Agent

#### Added

- **New experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature toggled in settings, off by default. When on, noa (model-driven context compaction) takes over context compaction for the main agent, planner, worker and chat agents, replacing the built-in compaction; the compacted original text is archived persistently under `<workDir>/noa/<session ID>/` (centralized rather than scattered across task directories), in a directory globally unique per session ID. A failed hookup falls back to built-in compaction automatically without interrupting real tasks; the switch is read once per run, so toggling only affects runs started afterwards.

#### Fixed

- **Exploration-graph tools refuse without a task context instead of crashing on nil**: calling exploration-graph tools outside a task context returns a clear error rather than dereferencing an empty store.

### MCP

#### Added

- **Support for legacy SSE MCP services** (#117): compatible with MCP servers offering only the old SSE transport.

### Traffic

#### Fixed

- **Traffic search matches recorded hosts port-aware** (#114): `traffic_search` host matching now includes the port, preventing records for the same host on different ports from cross-contaminating.
- **A failed `traffic_search` description migration no longer interrupts subsequent reporter migrations**: a single migration failure is isolated and does not affect later migrations.

### Web

#### Changed

- **Divider added between finding retest options** (#122): retest finding choices are now separated for clearer visuals.

#### Fixed

- **Fixed a full-page crash on the demo task detail page**: in mock mode the task detail "Sessions" tab fetches `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, so the read fallback treated it as a collection ("paths ending in s") and returned `[]`; that left `data.items` as `undefined`, the side hook's `merge()` threw `TypeError: t is not iterable` while iterating it, and because the exception happened inside a `setItems` updater React re-threw it in the render phase where the caller's `catch` could not reach it — the whole page was taken over by the error boundary showing "This page couldn't load". The mock handler now returns an explicit empty side history, and `sideAPI.history` normalizes the return value (`items` non-array always falls back to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **New DeepSeek official source for web search**: reuses the currently active LLM profile. It differs in nature from the other three sources — DeepSeek has no directly callable search API; search exists only inside its Anthropic-compatible endpoint (`web_search_20250305` server-side tool), executed by DeepSeek's servers. It therefore **only supports DeepSeek's official endpoint + the anthropic protocol** (OpenAI-protocol endpoints reject server-side tools outright), and each search **costs an extra model call**, its requests **bypass the search egress proxy** and are **not recorded as traffic**, and results contain **only titles and links** (no snippets; fetch the page with WebFetch when needed). The settings page documents these limits but **does not validate or block**; whether your setup qualifies is up to you to confirm, and the "Test search" button runs one live search to verify.

### Agent

#### Added

- **Workers gained cross-work look-back**: new `search_all_worker_traces` (search matching steps across every work in this task by keyword, without knowing an intent_id first) and `get_worker_trace` (lock onto one work, list its step stream, search by keyword in place, fetch full content by step_id), for reusing observations another work saw but never wrote into a fact, avoiding duplicated effort.
- **`node_detail` handed down to workers**: together with the look-back tools, a worker with an intent_id / node id can fetch that node's full detail directly.
- **Planning rounds triggered by `add_hint` are announced explicitly**: adding a hint previously only folded it into the situation overview for the planner to notice; now one `add_hint` records a "human added N strategic hints: …" trigger for the planner (batched into one, not one line per hint), and the planner is explicitly told "this round was triggered by a new hint" and sees the hint content directly.

#### Changed

- **Global situation prompts reworded to "loosen up"**: encourage exploration breadth and prompt reporting of cross-intent leads rather than converging early.
- **Tightened worker boundary wording**: initial obstruction does not mean exhausted; focus on "work through the bypasses inside this intent before concluding".
- **`insert_assets` dropped the per-asset `related` argument**: its only purpose was deciding whether to add the asset to this task's scope, and the value was not persisted anyway (registering the same asset again voids it, and the UI cannot show what was judged unrelated) — it merely made the model perform one more judgment that leaves no trace.
- **Test scope (`task_scope`) no longer depends on the asset-coverage switch**: `insert_assets`' automatic scoping (`source='auto'`) runs whether or not coverage is enabled, and `add_task_scope` is always offered to the planner / in-task main agent / goal-decomposition agent. Scope is the task's authorization boundary and the filter basis for asset queries; the coverage switch only decides whether to use it as a denominator for metrics, not whether the scope itself accumulates. Previously disabling coverage broke both the `auto` and `agent` write paths, leaving `task_scope` with only rows added by hand in the UI. `list_untested_assets` still hides when coverage is off (it is a pure coverage view).

#### Fixed

- **Fixed assets cross-contaminating between tasks** (#59): `list_assets` hardcoded the task id to 0 and queried the whole shared asset database bare, while the model had no way to write scope filters, so it treated other tasks' assets (especially IPs) as this task's targets and skewed the testing direction. It now returns only assets within the test scope (`task_scope`) of [this task and its directly related tasks], matched by **attribution** rather than literal value: a root domain in scope surfaces all its subdomains/services/endpoints, a network range surfaces the hosts and services inside it; fetching an out-of-scope asset by id also returns nothing. Non-task contexts (Auto/pentest) have no scope and still fall back to the full database. This also fixes the attribution blind spot where an IP-direct host (like `http://1.2.3.4/api`) could not be matched by a CIDR scope. The UI's "test assets" view (filtered by task producer) is unaffected.
- **Fixed the worker cross-work look-back tools being removed by mistake**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker default toolset, an old "tighten the worker tool surface" migration unbound them again at startup, so workers never actually had them. They are removed from that unbind list, and databases that already ran the old migration get them re-bound to workers once.
- **Fixed occasional `/btw` side-question write failures**: the side_question checkpoint now strips the NUL (`\u0000`) escape JSONB does not support before persisting, so content containing that character no longer errors on write.

### Triggers

#### Fixed

- **Merged trigger sessions deduplicate task descriptions/goals per task**: when multiple tasks merge into one trigger, each task's description/goal is no longer repeated in the message, preventing long goals from stacking up and blowing out the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main agent supports **multiple sessions**: create, switch and independently reset context, with each session interactive.
- Persistent `/btw` **side questions**: ask follow-ups without interrupting the main line; Q&A is persisted and survives restarts.
- `spawn_task` gained `source_task_ids`, letting a new task **read-only inherit** the source task's assets and findings.

#### Changed

- Disabled the worker's cross-engagement memory and tightened its default tool surface: reading context and cross-work retrospection are planning duties; a worker only executes and writes back a single intent.
- Slimmed the worker default prompt; `get_worker_output` dropped `terminated` and `worker_name`; tightened the `insert_assets` / `list_assets` descriptions.

#### Fixed

- **Fixed work being cut short by an idle turn**: the model sometimes spends a whole turn emitting only thinking, with neither text nor a tool call; the harness sees a natural end (`end_turn` with no `tool_use`) and finishes with `completed` + an empty summary — an unfinished intent broken halfway, showing up as "the task ended normally but produced no written summary". None of the five LLM retry layers reaches it: it is not an error, the same-provider safety-window retry's entry is a "stream failure", the breaker judges success outright when `err == nil`, and intent re-run only recognizes `model_error`; the SDK's empty-response retry cannot reach it either, because that layer judges emptiness by "did it yield any events", and thinking deltas are events. The work Stop hook now recognizes such a turn and injects a continuation instruction, letting the model carry the thinking it already produced into the next step — deliberately not "resend as-is", because these idle turns are usually determined by the prompt and context shape: stable behavior, not random flakiness, and a resend would just make the model think again.
- The count reuses the **empty-response retry count** from the LLM page's "Retries and backoff" (both answer "the model finished without producing substance", differing only in the criterion and remedy), default 2, and `-1` disables it, reverting to the old "an idle turn ends the work" behavior. It is a **total** cap per intent, not a consecutive one: the harness already limits consecutive idle turns to one push, and the quota only refreshes once a real tool turn has happened, so this number guards against loops like "tool → idle → push → tool → idle" draining the intent budget. On trigger the log records `[work <worker> · #<intent>] idle turn… injecting continuation instruction (n/N)`. Design: `docs/LLM-retry-design.md` §1.1.
- Fixed `/btw`'s context budget and input layout in long conversations; non-secure contexts (non-HTTPS access) fall back to a generated side-request ID.

### Traffic

#### Added

- Findings support **linking multiple traffic evidence records**, sortable, with notes and roles (request/response/supporting).
- The report agent **auto-links** relevant traffic evidence before writing a report.
- New agent **traffic-binding switch**, plus evidence handover between agents.

### Tasks

#### Added

- **Session-level finding retest**: retests run in a separate agent session, with the list and detail showing run state.

### Interception

#### Added

- Approval records gained **details and execution audit**: view the tool request context, the model/rule initial verdict, execution output and argument digest.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the test connection not sending the custom session header, which made opencode zen return 400.

### Network

#### Added

- MCP's HTTP transport supports **skipping TLS certificate verification**, for services with self-signed certificates.

### UI

#### Added

- The conversation list groups by agent, with expand/collapse and independent pinning; chat can be filtered by agent.
- The attack-path graph renders digest nodes and collapses their members.

#### Fixed

- Fixed the blank screen caused by out-of-sync login credentials.
- Intercept notices now explicitly say "platform control", so they are not misread as target-side defenses.

### Deployment and updates

#### Added

- **One-click update from the page**: a "Version and updates" card on the settings page, with the top bar lighting up when a new version exists. The flow is: download the release → verify `SHA256SUMS` → smoke test → stage → exit so the supervisor script relaunches into the new build, with the page refreshing automatically.
- New supervisor scripts `start.sh` / `start.bat` as the official entry points (shipped in both the release package and the Docker image; `install.sh` unchanged); they decide whether to relaunch based on the exit code and forward SIGTERM to artex. Verification and swap logic live in Go; the scripts stay dumb.
- Automatic fallback on failure: a failed verification or smoke test is discarded and the current version keeps running; three consecutive failed starts of a new version roll back to the previous one. The settings page also offers a manual rollback (note the database schema does not roll back).
- Updates accept only GitHub domains and force HTTPS; the release source is not configurable, and one-click update is disabled on dev builds. GitHub query results are cached for 30 minutes so the top-bar notice cannot exhaust the API quota.

#### Known limitations

- Under Docker only the program is swapped, not the image: the toolchain does not upgrade with it, and rebuilding the container reverts to the image's bundled version — use `docker compose pull artex` when needed.
- The release package's `skills/` is not synced, so built-in skills added in a new version do not take effect automatically.
- Updating restarts the process and interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- New "Retries and backoff" tab on the LLM page: the **count** and **interval** of all five retry layers are configurable. A failed model call passes, from inner to outer, through connect retry (SDK; connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK; finished normally with no content, openai format only), same-provider safety-window retry (replay a dropped stream before any output was delivered to the caller), pool breaker (cooldown-skip after consecutive failures hit the threshold) and intent re-run (re-run the whole intent after a worker ends with model_error); the outer layer only runs when the inner ones are exhausted. Each layer has two knobs with uniform semantics: empty = the original default count and exponential backoff; a count uses that count; an interval replaces exponential backoff with a fixed interval; `-1` disables that layer. The first three layers follow the endpoint and can override the global defaults field by field in each model profile (interval-only overrides still inherit the global count); the breaker and intent re-run are process-level semantics with a single global copy. Changes take effect hot, no restart. All-empty equals current behavior, and existing databases are byte-for-byte unchanged after upgrade (new columns default 0, missing settings keys mean all defaults). The connection test deliberately omits these parameters — it has a 30s hard timeout, and layering user-configured retries on top would turn a working endpoint into a timeout failure. Design: `docs/LLM-retry-design.md`.
- Each LLM profile supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries that HTTP header with the current run's session id (`conv-<id>` for chat sessions, `exp<x>-worker-i<intent>` for workers, etc.), for gateways that do prompt caching / sticky routing on a session-id header. It reads the session id from the request context and injects it — no norma changes needed, and the same shared provider can emit different header values per session. Existing databases get a migration, and the LLM profile dialog gained an input field.

#### Fixed

- Fixed saving an LLM profile erroring with `23502` when the session-header field is empty: the column is `NOT NULL DEFAULT ''`, and the code wrongly applied `NULLIF($n,'')`, writing an unfilled session header as NULL and tripping the not-null constraint; it now passes the parameter with empty-string semantics.

### Agent

#### Added

- Wall-clock timeout now **wraps up in place**: relies on norma v0.3.6; when `MaxDuration` expires it interrupts the running tool and continues on the live ctx for the wrap-up turns (writing back identified content + a summary, terminal state timeout), no longer relying on the worker/planner `maxDur+90s` external hard ctx that killed stuck runs as `aborted_tools`. Chat shares the same harness and gets the same behavior automatically; mainagent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner is woken by heartbeat / no-change and the whole graph has no open or running intents, the opening line becomes a stall warning that clearly states no worker is running and nothing is queued, and this round must produce one or more non-duplicate new intents (0 intents is not allowed).

#### Changed

- Worker prompt rebuilt: the intent / start instruction / anchored-asset raw JSON move into the system prompt, reassembled every round and never squashed by compaction, and continuation no longer depends on the transcript's first message surviving; the start user message slims down to just the global situation overview (degradable, stale-tolerant). The cost is per-intent data mixed into the system prompt and loss of cross-intent cache reuse — a deliberate trade for "never lose the intent".
- Slimmed the planner / worker default text and fixed several real-world issues: the planner distinguishes `recent_done` by state (blocked/exhausted check the trace first rather than treating it as a dead end or blindly re-running), negative conclusions become "observation / open question, not final" and evidence must be read before believing them, an unmet goal with no open/running intents must produce one (hard floor), and depth outranks coverage; workers write negative conclusions only as "observation + tentative reading" leaving the verdict to the planner, and cross-intent leads go into the fact's summary for the planner rather than being chased. Reseed appends the new defaults as a new version and switches to it; user customizations / old versions stay in history and can be rolled back.
- Tightened the work agent's default toolset and asset write-back hints: a worker only executes and writes back a single intent — reading context and cross-work retrospection are planning duties — so `list_facts` / `node_detail` / `list_companies` and the cross-work search tools (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) were removed from its defaults, leaving `list_findings` (dedupe before reporting) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt dropped stale field descriptions that conflicted with the `insert_assets` schema (`type=tech` / `on_url` / `props`). Existing databases get a one-time migration stripping those bindings; the planner/main bindings of the same names are untouched.

### Exploration graph

#### Added

- **Cold-node compaction in the exploration graph (cold-digest)**: old, long-inactive intents / facts are folded into digest nodes for display in `graph_overview`, while the original nodes are kept forever and fully restorable by id (lossless storage, presentation-only compression, reversible folding). Cold/hot is decided by reverse reachability + any live branch = hot, with R=6 rounds of debounce and connected-component grouping; background compaction (minor folds uncovered cold blocks, major re-sources and merges fragments) re-checks activity and mutexes on cooldown, never touching the hot path and never covering a revived node. The overview exposes `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` restoring them. Related / inherited task overviews reuse their own folded views, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` go to the planner and main agent only, not workers. All with existing-database migrations.
- `graph_overview` now outputs the full `finding_list`: findings are the task's highest-value output and usually few per task, so they are now included in full (unlike facts, which get only the recent window), letting planner/worker see every confirmed finding each round without calling `list_findings`. Each entry is slimmed to `{id, summary, evidence?, from_intent?, assets?}`, with affected assets given as readable content (url / domain / ip:port) rather than bare ids.

#### Changed

- `graph_overview` no longer flattens `hosts`: the coverage block drops the host list (a large-scope task would carry up to 500 host strings per round for limited planning value), keeping only `host_count`, with specific hosts queried via `list_assets` on demand. A new top-level `done_intents_total` (total finished intents) sits alongside `recent_done_intents` truncated to ≤15, so the planner knows whether anything was cut when deduplicating.

### Tools

#### Changed

- `add_company_scope` default binding moved from worker to planner: defining company asset scope is a planning / control / Auto duty; workers only execute exploration. New databases seed it to mainagent/planner/auto, existing ones get a one-time migration.
- The planner now binds `list_assets` by default: it has both `list_assets` (DSL search over the whole database) and `list_untested_assets` (untested in scope); existing databases are backfilled once without overriding user unbinds.

### Tasks

#### Added

- New "running workers" column in the task list: counts intent nodes with `state='running'` under that task, exactly matching the task detail overview's "running workers", showing 0 when none.

### Network

#### Added

- New **global egress proxy** setting: all target traffic can egress through one global proxy (http/https/socks5, `user:pass` supported). With traffic capture on it becomes the MITM recording proxy's upstream (traffic is still recorded and then egresses via the proxy; both intercept and passthrough paths use the upstream, not leaking the source IP); with capture off it is injected into the agent's bash environment and WebFetch (`proxyEnv` gains `ALL_PROXY` for socks5). Stored in the settings KV table (no migration); the settings page gained a "Global proxy" card, independent of the web-search proxy and the LLM proxy.

### UI

#### Fixed

- Corrected semantic errors in intent state labels: `exhausted` "exhausted" → "budget exhausted" (it actually means the step/time budget cut it off mid-way with partial results, not that the direction was fully explored) and `blocked` "intercepted" → "execution error" (it actually means model / API / network failures exhausted retries and the intent barely explored, not a target / WAF interception), plus the previously missing `stopped` "stopped" (a work stopped manually by the user).

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gained truncation and spilling (see `651b961`); the later v0.3.6 supports in-place wall-clock wrap-up.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model profiles gained an "output cap" plus a choice of which request field name carries it: the cap limits how many tokens one reply may generate, sent with every request; 0 (default) = do not send the field, leaving it to the server default. It is not the same as "context window" — that is the model's total capacity, used locally only to compute the compaction threshold and never present in requests. The field-name switch only matters for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which nearly every compatible gateway accepts; OpenAI's official reasoning models (o series / GPT-5) conversely accept only `max_completion_tokens` and reject `max_tokens` with `unsupported_parameter`, so those endpoints must switch manually. The other two formats pin their field names (Anthropic uses `max_tokens`, Responses API uses `max_output_tokens`), so the switch is greyed out for non-openai formats and cleared on save. This also connects a line that was never wired: planner / worker / chat / main agent / goal decomposition never set an output cap, so openai-family endpoints sent none and Anthropic fell back to the SDK's 8192 — adding the setting without this wiring would have made the number do nothing; the value is now resolved per round like "streaming output" and takes effect on the next round after a failover switch. Existing databases behave exactly as before after upgrade (new column defaults 0 and an empty field name).
- Clicking "Test" in the backend now logs each HTTP attempt's status code and the gateway's raw response body to the server log (bodies truncated to 4K), making 401s, quota messages, empty frames and HTML pages diagnosable instead of only the collapsed ok / err in the UI. This logging does not depend on the "LLM recording" switch.

#### Changed

- The task LLM profile chain is now editable in any state, no longer restricted to "running / paused / chain exhausted": after a task ends (done / failed / timeout) the main agent conversation still uses this chain, and a problem with a model on it previously left no way to fix it or keep interacting. The terminal-state blocks in both the HTTP and DB transaction layers are removed, and the frontend dialog allows editing and saving for terminal tasks too. Saving on a terminal task no longer reopens quota-blocked intents (those would be moved to open without a worker to run them and no longer meeting the "re-run intent" conditions); to continue, use re-run intent / add goal, which pull the task back into running.

### Agent

#### Changed

- "Message a running worker to redirect it" now reuses the existing pause / resume + transcript-resume mechanism, behaving like a main-agent conversation: it previously used a bespoke intervention-persistence protocol touching the scheduling barrier, the recovery flow and a dozen activity-query filters. Messages are now injected as the next round's input, the intent runs directly in its own goroutine, unconstrained by the 3-slot worker pool and running as soon as sent; the frontend keeps the message box and restores the "continue directly" button, with delivery carried over SSE. No DB schema change. Trade-offs: messages live only in memory without crash recovery, and sending may momentarily exceed the task's worker concurrency by one (a low-frequency case, acceptable).

### Tasks

#### Fixed

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archiving now streams the snapshot instead of reading the whole task into memory at once; three recovery gaps on the cold-archive path are also closed — modern installs keep traffic only in SQLite, and a crash between the PostgreSQL and SQLite commits used to leave an archived task's traffic stranded in hot storage with no way to recover. A staging journal is now written whether or not a legacy directory exists, and a missing `journal.json` is treated as discardable.
- Fixed the system stalling when there are many tasks: loading conversations and loading the UI had noticeable latency. Queries for the task list, task context and exploration records were optimized together, and the dashboard, chat page and task detail page reduced duplicate requests in step.

### Assets

#### Changed

- Removed the "at most 256 rules" limit on company asset scope: companies entering scope IP by IP / domain by domain hit this limit easily, and once hit they had to split into multiple companies even though a scope is not slower for having more rules. The limit is removed in all three places (frontend, backend, demo mock); a single rule is still capped at 1024 characters and the 2 MiB request-body limit stays as a backstop (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed skill-zip uploads failing with "upload failed: zip: unsupported compression": Go's standard library only ships the Store / Deflate decompressors, so bzip2 and Zstandard archives written by compression tools at non-default settings never opened. Both decompressors are now included (pure Go, no new external dependency); genuinely unsupported methods like Deflate64 / LZMA / XZ / PPMd, and encrypted archives, now produce a clear message before extraction naming which file used which compression method and how to repack, instead of throwing the underlying English error at the user.
- Fixed the in-skill filename check rejecting Chinese filenames: path validation used an ASCII allowlist of `[A-Za-z0-9-_./]`, so one Chinese-named file in the zip (like `参考/说明.md`) failed the whole upload with "archive contains illegal paths". It is now a Unicode blocklist: filenames in any language and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, `..`, absolute paths and empty path segments are still rejected, with directory-traversal protection unchanged. Skill names are similarly relaxed — ASCII is still limited to lowercase letters, digits and hyphens (agentskills.io spec), and non-ASCII letters such as Chinese are allowed directly as skill names, but spaces, dots and path separators are not accepted.
- Fixed Chinese archives from Windows compression tools extracting with garbled filenames or being rejected wholesale: such zips do not set the UTF-8 flag and write filenames in GBK, which is now decoded as a fallback before path validation. Quoted frontmatter like `name: "中文技能"` also resolves the skill name correctly now.

### UI

#### Added

- The findings page gained a flat "all findings" view as the default, switchable via header tabs from the original "grouped by task" view: the grouped view requires expanding each task to see findings, a detour when you just want to scan the list across tasks. The flat view is one big cross-task table (10/20/50/100 per page) behaving exactly like the grouped view — select to export, expand a row for evidence and the detailed report, edit name/class/severity inline, change disposition status, deepen, delete. Both views share the stat cards and filter bar, switching preserves filters, and the current view and filters are stored locally; polling only hits the current view, while inline changes write to both caches so switching never shows stale data.
- The findings page gained a "by asset" view: an asset tree on the left (company → root domain / IP → subdomain → service → endpoint, with the company level appearing only when an asset really has an owner) and the findings under the selected node's whole subtree on the right, sharing the same table and filter set as the other two views. The tree includes only assets with findings, filling in ancestors on demand (when a finding hangs only on the deepest endpoint, the upper levels are still assembled), and node counts aggregate the subtree deduplicated by finding; findings whose asset was deleted or which have none go into the "unlinked assets" bucket. Unlike the other two views, the asset view does not poll — it queries when you enter the view, change filters, add/edit/delete a finding on the page, or click the tree's refresh button; the left tree is a navigation structure and need not be recomputed every 5 seconds. Each tree level shows only the increment relative to its parent (subdomains drop the root-domain suffix, services show `https :443`, endpoints show only the path), with full values in the tooltip and breadcrumb; when a level is too large the endpoint/service levels are dropped wholesale with a notice, and counts still roll up.

#### Fixed

- Fixed the conversation record being squeezed and unreadable in the mobile session detail: on narrow screens the conversation list collapses, giving the width to the record itself.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: adds `Config.MaxTokensField`, letting OpenAI Chat Completions send its output cap as `max_completion_tokens` (reasoning models accept only that key). The two keys are mutually exclusive and only one is sent; the default remains `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" toggle reverting to "streaming" after saving and reopening (#69): the profile-list endpoint's DTO was missing the `streaming` field, so the response never returned it and the frontend read `undefined` and fell back to the default streaming; writes and DB storage were fine, only reads failed. The DTO now includes the field (without `omitempty`, so `false` also appears in responses).
- The connection test now verifies the model actually replied and tests with the profile's real request/response mode (#65): previously it only judged whether HTTP succeeded, so a request that went through with zero model output (thinking burned the whole budget / content swallowed by a safety policy / a compatibility layer dropping `content`) still reported "connection successful", contradicting the "no reply" behavior in conversations; and the test always used streaming, so a non-streaming profile was really testing a different channel. An empty reply now fails outright, a successful test shows the model's reply, and the current streaming toggle is carried into the test, keeping "test passes" consistent with "the conversation actually works".

### Agent

#### Changed

- `list_facts` became paginated with keyword filtering, so a single call no longer blows the context when there are many facts (#74): returns the latest 20 by default, supports `limit` (max 100) / `before` cursor / `q` summary keyword, and returns `{facts, total, has_more, next_before}`; an overlong single summary is truncated by character count (the full text remains available via `node_detail`). The worker / planner prompts were updated to the paginated semantics. Existing databases get a one-time migration refreshing the new parameter schema into the tool catalog table (`SeedTool` is insert-only, or the tools page would show "no parameters").

### Tools

#### Added

- New tool invocation-count stats on the "Tool runs" page (#72): the toolbar's "Stats" button opens a dialog showing per-tool call counts, share and failure counts (descending); it reuses the list's task / keyword filters, counts the whole result set rather than the current page, and fetches only when the dialog opens.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixes silently dropped reasoning/refusal in OpenAI-family responses (Responses gains `reasoning_text`), deduplicates the three reasoning field names, bounded retry on empty responses (zero content blocks), and gateway 400s caused by a compaction boundary cutting tool pairings (also repairing existing orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Every LLM profile supports a streaming/non-streaming switch (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, one full JSON response), bypassing some gateways' poor SSE implementations (empty frames, dropped thinking fields) at the cost of losing live progress and live token counts. Worker / planner / mainagent / chat / goals all resolve it dynamically from the active profile; all three provider wrappers (`llmpool` / `llmrec` / task runtime) are non-streaming compatible; `llm_profiles` gains a `streaming` column with an `ALTER` for existing databases (default `true`, invisible to old profiles).
- Support for LLM profiles using the OpenAI Responses API format: a third format `openai-responses` (hitting `POST /v1/responses`) alongside Chat Completions / Anthropic; `BaseURL` normalization, default model `gpt-5`; the `llm_profiles` `format` constraint accepts `openai-responses` with an idempotent migration for existing databases; the frontend format dropdown gained "OpenAI (Responses API)". Dependency upgrade to norma v0.3.1 (including the `reasoning_content` pass-through fix).
- Recording raw LLM request/response HTTP: captures the real wire body at the HTTP transport layer, preserving the tool schemas, `tool_use` blocks and raw SSE frames the normalized view cannot show; norma's internal retries are kept attempt by attempt; `llm_records` gains `raw_request` / `raw_response` columns with an `ALTER` for existing databases; the recording page's detail panel gained a "raw" view toggle and request/response copy buttons (with an `execCommand` fallback for non-secure contexts).

### Chat

#### Changed

- Conversation-list multi-select became a "multi-select" mode toggle: the default list no longer shows a checkbox on every row (cleaner look), and the header shows "N total + Multi-select"; clicking it enters selection mode (checkboxes, select all, bulk delete), "Done" exits and clears the selection, a fully successful bulk delete exits automatically (any failure keeps selection mode for retrying); single rename / pin / delete still live in each row's ⋯ menu.

### UI

#### Changed

- Enhanced task management, conversation operations and traffic viewing (#57): task-list column sort preferences persist, inline rename is triggered directly by an icon (no longer through the menu), and sheet interactions and traffic-view details were polished.

#### Fixed

- Worker asset labels show only the domain and IP, and empty labels are handled correctly.

### Agent

#### Added

- The planner gained a "quantitative acceptance check" when judging goals: when a goal has quantifiable acceptance criteria (asset test coverage reaching X%, N flags captured, a certain privilege obtained), `prove_goal` must first check the measured values in `graph_overview` (`coverage.pct`, finding counts, etc.); if the measurement falls short, `prove_goal` is forbidden and intents must be dispatched to close the gap — "the main part is done / roughly achieved" is not grounds to mark it met early. This fixes the case where a goal required 100% coverage, only 40% was measured, and it was still judged complete.

#### Changed

- Rewrote the basis for "0 intents this round" in the planner prompt: the original described 0 intents as "the most common, most important principle", which made the planner give up early when goals were unmet and untested surface remained in scope. It now produces 0 intents only in two specific cases — ① every direction it can think of is already covered by open/running/recent_done intents; ② the next step depends on output from a currently running work that has not appeared yet (it should wait for that work to finish and plan on the next wake after the graph updates). The reverse constraint is also spelled out: when a genuinely uncovered new direction exists that does not depend on a running work, or goals are unmet with untested surface remaining, do not stop because "0 intents is common".
- Strengthened the "evidence bar for negative conclusions" in the worker prompt: for negative conclusions that could make the planner abandon a whole direction (not injectable / port closed / no login entry), the worker must exhaust reasonable means within the intent (alternate encodings/parameters/paths/methods) before concluding; if the means are not exhausted or the evidence is weak, it must mark `confidence=inferred`, avoiding a careless `observed` negative that welds a route shut (an early wrong negative especially skews the direction and is hard to self-heal).
- The planner / worker default prompt changes above were appended as new versions via one-time migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag) and switched to as current; old versions stay in version history, and users who customized prompts can restore them from the version records.

### Operations

#### Added

- New `reset-password.sh` to reset the administrator password (username fixed to `ARTEX`): supports both local and docker deployments, with connection info given explicitly or read automatically from `--dsn` / `$ARTEX_PG_DSN` / `config.json`; it generates a backend-login-compatible bcrypt hash in-database with `pgcrypto` and writes it back to `settings.auth.password_hash`, with no restart needed after resetting. The password is passed via environment variable (never entering the process argv) and escaped against injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- The task list supports bulk category changes after selecting multiple tasks, with "uncategorized" as a target to move tasks out of their current category; the whole batch commits in one transaction, and the only possible failures are tasks deleted after selection.

#### Changed

- The new-task "task category" field became a searchable input: typing filters existing categories, an unknown name is created and selected on Enter (or by clicking "Create" in the dropdown), and the selection shows as a removable tag; still one category per task.

### Chat

#### Fixed

- Fixed messages being blocked with "LLM not configured, cannot chat" when a conversation has a selected LLM profile but no global active profile. The pre-send check only looked at the global active profile while execution actually preferred the conversation's profile — two inconsistent resolution paths; they are now one shared path (conversation/agent binding first, global as fallback).
- The "cannot chat" notice is now split by state: no profiles at all says "add a profile", profiles exist but none active says "activate one or pick one for this conversation", instead of always saying "not configured", making the cause easy to locate.

### Agent

#### Added

- On tasks whose goals are all achieved, intents dispatched by the main agent via `add_intent` can be claimed and run by workers and stop when done: the planner no longer runs then (a task with no open goals does not enter planning), preventing it from re-judging goals met and canceling the freshly dispatched intent; once the frontier drains the task returns to completed. Before dispatching, the main agent asks the user whether to register the intent as a formal goal; registering returns the task to normal autonomous planning.

#### Changed

- `get_worker_trace` / `get_task_worker_trace` no longer error outright when `step_ids` exceeds the per-call cap (5); they return the full content of the first 5 steps and report via `returned_step_ids` / `omitted_step_ids` / `notice` which were taken and which were not, with the hint "if this is enough, no need to fetch more". Duplicate and invalid ids are deduplicated and dropped before counting.

#### Fixed

- The task list now sorts by task id descending (newest first), replacing the previous created-time sort. Multiple tasks created in the same instant share a timestamp, making the order unstable, and combined with 10-second polling and random in-memory map iteration they swapped positions frequently in the list.

### Assets

#### Fixed

- Fixed company-attribution recomputation being broken by a single dirty row: when `assets.ip` held a hostname (written by an agent or the asset API), `a.ip::inet` threw `22P02`, making add/edit company scope and company deletion all fail and roll back. It now uses the safe cast `try_inet()`, skipping invalid values instead of aborting the whole statement.
- Unparsable `ip` values are no longer silently skipped: after saving company scope, the response clearly states how many assets have an `ip` that is not a valid IP (with specific ids and values), warning that these assets will not be attributed by IP/CIDR rules; the same information goes to the server log, covering paths with no frontend response (company deletion, scopesentry sync, agent writes).
- Task test-scope IP/CIDR matching also uses `try_inet()`, replacing the previous regex guard that only checked the character set (`abc.def`-style all-hex hostnames slipped through and triggered the same error).

#### Changed

- The `ip` field of `ip`, `service` and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return per-item errors with an `index` and the fix directly (use `type=subdomain` with `domain`, or resolve the A/AAAA record first), letting the agent correct and re-insert. Other assets in the same batch are unaffected and store normally.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running workers support per-item pause, resume and cancel; pausing keeps the intent, facts and findings, while canceling transactionally cleans up the current intent and its direct output after execution exits.
- Added a `paused` intent state, execution fences and named termination reasons, preventing late blackboard writes after a pause, cancel or task deletion.
- Added an optional task concurrency cap; create, restore, finding-deepen and queue refills all go through the persistent FIFO admission path.

#### Changed

- The worker's default single-run wall-clock duration changed from 600 to 1200 seconds; a timed-out task restarts the clock on its next real execution.
- The real execution state of workers, the planner and the main agent now uniformly drives the task status badge, fixing "idle" showing while a worker was running.
- Worker sessions keep per-item control and the current session's token totals, with the model name moved next to the current session title.

#### Removed

- Removed the worker multi-select, select-all and bulk pause/resume UI, along with the worker bulk-control API and mock contract.
- Removed the token badge and tooltip from worker list rows; the full underlying token ledger and task aggregation endpoints remain.

### LLM

#### Added

- Tasks support an ordered LLM profile chain; on a clearly identified provider quota shortfall it switches to the next profile automatically, persisting the current profile, exhaustion state and error summary.
- Running and paused tasks can edit the full chain, reorder it and switch the current profile manually, taking effect from the next LLM call.
- Automatic switches, manual switches and full-chain exhaustion are written as structured system activities and surfaced as deduplicable toasts in the task activity stream.
- Added a task role-model resolution endpoint that uniformly resolves the profile and model the main agent, planner and worker will use on their next call.
- Added an optional global LLM pool with configurable call order, bound-model failover, health state, cooldown recovery and manual reset.
- Added an always-on LLM usage ledger aggregating input, output and cache tokens by task, conversation, model and profile.

#### Changed

- Goal decomposition, the planner, workers and the main agent share a task-level LLM runtime (resolution priority below).
- Failover only responds to explicit quota, balance or billing errors; ordinary rate limits, auth, network, server and context errors do not trigger a wrong switch.
- The LLM page became a card list with a right-side drawer editor, offering pool order, priority, exclusions, health state and recovery actions.
- Chain-exhaustion messages wrap on narrow screens; the current model moved from a conversation-list icon to a text label with a full tooltip beside the current session title.
- The in-task role LLM resolution order is now "agent binding → task chain → global": roles explicitly bound to a model always run on it, unbound roles fall to the task chain, and an empty chain falls to global.
- An empty egress proxy means direct connection, no longer falling back to `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); proxy input accepts `socks5://user:pass@host:port` with credentials.

#### Fixed

- Transient streaming failures before commit (no output produced yet) retry safely with backoff on the same provider, significantly reducing "broke off after one or two tools, no summary, terminal `model_error`"; quota exhaustion, overlong context and deterministic 4xx errors are not retried and stay with failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon indicator in the conversation list to avoid confusion with worker status icons.

### UI

#### Added

- Tasks support global categories with create, rename, delete and filter; a new task can pick a category directly.
- Added global task-template CRUD with a right-side management drawer; a new task can load a preset description and goal, or save the current content as a template.
- New tasks can link multiple source tasks and multiple company asset scopes; the company scope uses one multi-line text box that auto-recognizes domains, URLs, IPs, CIDRs, ICP numbers and company keywords.
- Task test assets support adding and removing in place, recording the source (manual, company, inherited or agent-discovered); worker session titles show the current test assets and a source summary.
- The findings page groups by task with independent in-group pagination, and supports writing a description for a finding and creating a high-priority deepen intent.
- Conversations support rename, pin, unpin and delete; the task list supports bulk pause/resume for the current page.
- Traffic detail and tool-run detail moved to right-side drawers; HTTP messages fill in `Host` and highlight the request line, status code, headers, JSON and markup bodies.
- The task list's action column gained "Details" and "Pause/Resume" buttons; the conversation send keybinding is configurable in settings (`Enter` / `Cmd+Enter`, etc.), and the web-search section gained a proxy input.
- The badge beside a conversation title now shows the LLM profile name with the model id in a hover tooltip; tasks can have an optional name, with a name column and search in the task list (falling back to the description when empty).
- The skills page gained usage counts, last-used times and missing-dependency stats, making inactive or uninstalled skills easy to spot.

#### Changed

- Task titles became focusable detail links; task assets, company assets, finding task groups and in-group findings use real server-side pagination, stable ordering and exact totals.
- Creating a company moved to a right-side drawer, with scope entry unified as multi-line text offering live recognition, validation and type preview.
- The main-agent input box auto-grows, sends on `Enter` and newlines on `Shift+Enter`, and avoids mis-sending while an IME is composing.
- Agent previews, task reports and related details uniformly use the shared Markdown renderer; delete confirmations, long errors and mobile drawer widths were fixed consistently.
- The task-template picker displays and searches by template name and submits by template id; the system font size was restored to its original, and the app version shows uniformly as `0.3.3`.
- Task totals and current-session tokens highlight only input, cache-read and output values; the send button uses a cleaner up-arrow icon.
- The task overview's test scope shows company names rather than ids.

#### Fixed

- Ordinary text containing a dot is no longer mistaken for an ICP filing number.
- Fixed variable-directory entries colliding with global runtime variables (like `{{.Now}}`), which rendered duplicate keys in the agent editor's variable list.

#### Removed

- Removed the separate "enter" button on task cards; clicking the task title opens the detail uniformly.
- Removed the dashboard's "new task" button, the company logo-URL input, and the finding-count badges at the top of the findings page and in task groups.
- Reverted the global 10% font-size increase and removed the worker select box, model icon and row-level token stats from the conversation list.

### Agent

#### Added

- New tasks can directly link multiple existing tasks, read-only inheriting in real time the direct source tasks' goals, facts, findings, finished intents, asset scope and blackboard context.
- Blackboard read tools can query a source task's nodes, facts, findings and execution traces on demand; inherited nodes carry a source marker and every write tool refuses to modify them.
- Company-scope tools support domains, URLs, IPs, CIDRs, ICP numbers and company keywords; keywords serve only as agent scope hints and do not participate in automatic asset attribution.
- The finding-deepen action creates a high-priority manual worker intent in the original task, with asset anchors and `derived_from` edges.
- The overview gained a "goal management" card for viewing, adding, editing and deleting goals by hand; adding or editing notifies the planner and revives the task, while deleting hard-deletes the goal node (cascading edges and anchors) without reviving.
- The main agent binds the `steer_work` tool by default, injecting real-time course corrections into a running worker without interrupting it or losing progress (still verifying the intent belongs to this task).

#### Changed

- Source-task intents do not enter the new task's frontier; the new task keeps its own exploration, execution queue, working directory and conversation history.
- Deleting a running intent no longer destroys data: it stops to `stopped`, requires a deletion reason, hangs the reason on the intent as a fact and writes it into the payload, and the planner perceives it as a `cancelled` trigger (keeping the intent content and reason).
- Main-agent chat is now driven by the server-side activity stream, and a failed send restores the input; opening a conversation sticks to the bottom automatically and keeps the last reply visible after lazy detail loading.
- Pausing a task terminates the current main agent, planner and worker calls, but does not prevent the user from starting new main-agent orchestration sessions while paused.
- Cancels, closures and streaming interruptions uniformly preserve the real termination reason, generated content, rounds run, elapsed time, tokens and unreturned tool calls.

#### Removed

- Removed the main agent's client-side optimistic message echo, avoiding duplicate messages and cross-conversation bleed during pauses, failures or concurrent activity.

### Constraints

#### Added

- Goal decomposition now extracts operation constraints (allow/deny) before decomposing goals, and the main agent can add more at runtime; constraints are injected into the planner and worker system prompts at top priority, with diversity and surface-expansion exploration explicitly subordinate to them after de-biasing.
- The overview gained a "constraint management" card with add/edit/delete endpoints; the injection scope is switchable per planner / worker (both on by default, read every round and effective immediately). The new `task_constraints` table uses `CREATE TABLE IF NOT EXISTS` and is auto-created on existing databases.

### Interception

#### Added

- Command interception gained model-fallback approval beyond regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with configurable failure fallback and approval-timeout actions; the interception page split into "Intercept rules / Model config" tabs, and model verdicts are labeled with a `[模型]` prefix and a reason.

#### Fixed

- Hardened judge-output parsing so a correct `DENY` verdict is not mistaken for an allow.

### Traffic

#### Changed

- Traffic recording now stores each whole exchange in SQLite (large bodies spill to a hash-deduplicated blob bucket), with a trigram full-text index on text bodies supporting arbitrary substrings and Chinese search; deletion degrades to a single SQL transaction, dropping from hours to milliseconds without stalling recording. Added streaming download of large bodies and a `traffic_search` full-text parameter; legacy file-tree data needs no migration and remains readable, searchable and deletable.

### Tasks and assets

#### Added

- New tasks gained an "asset coverage feature" switch (on by default): when off, coverage is neither computed nor shown, the situation graph shows assets only, `task_scope` does not accumulate automatically, and the related tools are removed from the planner / main agent; company links are unaffected by this switch.
- `insert_assets` gained a per-asset `related` marker (default `true`): effective only when coverage is on; `false` stores the asset in the shared database only without counting toward this task's coverage (for incidental or unrelated assets).
- Company scope and task test assets uniformly support text recognition of domains, URLs, IPs, CIDRs, ICP numbers and keywords; domains/IPs can create or reuse global assets, while CIDRs/ICP/keywords stay as task-scope context.

### Build

- `build.sh --release` builds Linux amd64/arm64, macOS amd64/arm64 and Windows amd64 in one go, producing zip release packages with `skills/`, config examples and the README.
- Uses the Go linker to strip debug info and produce zip packages; UPX became explicitly optional to avoid its self-extracting ELF segfaulting at startup on some Linux environments.
- The release workflow runs a startup smoke test on the Linux amd64 binary and ships `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks can link multiple existing tasks, read-only inheriting in real time the direct source tasks' facts, findings, finished intents, asset scope and blackboard context; the new task still uses its own execution queue, working directory and conversation history.
- Tasks support an ordered LLM profile chain. On a clearly identified provider quota shortfall it switches to the next profile automatically, persisting the current profile, exhaustion state and structured audit activities.
- Running and paused tasks can edit the LLM profile chain, reorder it and switch the current profile manually; automatic switches, manual switches and full-chain exhaustion are surfaced on the task detail page.
- Running workers support pause, resume and cancel. Pausing keeps blackboard data; canceling transactionally cleans up the intent and the facts, findings and execution records it directly produced after the worker stops writing.
- Task deletion can optionally clean up associated assets, traffic, findings and task files, with a deletion barrier, concurrency protection and auditable deletion stats.
- Added an optional task concurrency cap; new tasks beyond the cap queue FIFO and start automatically when a run slot frees up.
- Added pagination for task assets and company assets, task-category stats, and a paginated intent mock contract.
- Added a `build.sh` single-binary build script supporting static frontend export, asset embedding, cross-platform targets and build-version injection.

### Changed

- The task-level explicit LLM profile chain coexists with the existing global LLM pool; the explicit chain keeps strict quota-failover semantics, and without one the agent-binding and global rules apply.
- The main-agent input box supports auto-growing multi-line input, sends on `Enter`, newlines on `Shift+Enter`, and avoids mis-sending while an IME is composing.
- The planner, workers and the main agent share the task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compaction threshold.
- The task detail page shows running, idle and paused states based on real LLM call state; source-task facts, findings, intents, asset references and graph nodes uniformly carry a source marker and stay read-only.
- Agent previews, task reports and related detail views uniformly use the shared Markdown renderer.
- LLM model profiles use a card-and-drawer interaction, with the model list scrolling independently inside the drawer.
- Pausing a task no longer blocks main-agent chat: main-agent orchestration sessions are independent of the pause, and new messages can still be sent while paused (pausing only terminates the round currently in progress).
- The worker's default single-run wall-clock duration changed from 600 to 1200 seconds.

### Fixed

- Removed the main-agent console's optimistic echo in favor of pure server-side rendering, fixing garbled messages and content bleeding into other conversations during pauses or failed sends.
- Opening a main-agent conversation scrolls to the bottom by default and fully shows the last reply: its full content lazy-expands and sticks to the bottom instead of being pushed off screen.
- Fixed the main-agent conversation continuing after a task pause, and orchestrating agents pausing a task without stopping the main agent in step.
- Fixed the task status badge and action-button states being out of sync when a task completes or when the planner, a worker or the main agent is actually running.
- Fixed late blackboard writes or leftover files possible between worker cancels, task deletions and concurrent writes.
- Fixed broken Markdown previews, long-text overflow in delete confirmations, mobile widths and misaligned buttons in some task detail views.
- Added named termination reasons to every agent cancel path, with activity details showing the canceller, terminal state, rounds run, elapsed time, token usage and unreturned tool calls.
- Fixed a race where the parent context could override a named `shutdown` reason during backend shutdown, and garbled text from byte-truncating Chinese activity summaries.
- Fixed cancel events with partial streaming output losing the real termination reason, while preserving content generated before the cancel.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
