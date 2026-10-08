# Long side-question conversations and the context budget

Fixed on 2026-09-11. The original implementation treated the character count of the request JSON directly as tokens and inherited the main task's 32K output reservation, so a normal side question was rejected prematurely whenever there was a lot of HTML, JS or tool output.

## Review of open source implementations

- [Grok CLI side-question context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts fragments from recent user and assistant text, with a character budget of about 2000 and at most 400 characters per entry. It keeps no continuous side-question history on this path.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): an independent cancellation signal; an output cap of 2048 tokens where the model supports it; no tools.
- [Grok CLI main session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, folds new content into the old summary, and handles truncation across turns.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): estimates the whole request, reserves output/buffer, keeps recent content plus a rolling summary, and makes tool-free summary requests; that implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is only attempted before assistant output has started, and the recovered call does not re-enter the same overflow recovery path.

ARTEX borrows the independent output budget, recent content plus a rolling summary, and bounded recovery. It keeps norma v0.3.6's structured messages and tool pairing rather than copying Grok's text extraction, and it does not write OpenCode's main-session compaction events into ARTEX's main transcript.

## Request budget and execution

- Messages reuse norma's per-content-block UTF-8 byte estimate with a 4/3 margin, plus the overhead of the system prompt, tool schemas and message wrapping. The estimate is not an exact model token count.
- Side-question output defaults to at most 8192 tokens and never exceeds the output cap already set in the main configuration. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` sets a cap between 256 and 32768; it never changes the product's default model or the main task parameters.
- The input budget is the context window minus the output cap and a safety margin; an unknown window uses the platform default of 200K. The safety margin is 5% of the window, with a minimum of 128 and a maximum of 8192 tokens.
- Successful question/answer pairs are loaded by increasing ordinal, at most 20 per batch. At most 20 pairs are kept verbatim, with a token budget of at most a quarter of the input budget and no more than 16K.
- Pairs beyond that are folded into the rolling summary. The summary carries its historical source and context time; a historical assistant answer is not the same as new tool evidence, and on a conflict the latest main snapshot wins.
- When the main context is still too long, only the older messages of the copy are summarized, keeping at most 8K tokens of recent content; the cut point never separates a tool call from its result. An oversized single pair goes into the summary as a whole.
- Summary input is split into UTF-8-safe chunks sized by the window actually remaining, with an output cap of 2048 tokens; an empty summary, a truncation, a tool call or exceeding the summary budget all skip the cache. One side question makes at most 12 summary calls and is bound by the same 120-second timeout; reaching the limit fails explicitly rather than looping forever.
- If the model reports a context overflow on the first attempt and no text or tool call has been emitted yet, it is retried at most once after further reduction; if the estimated size did not drop, recovery stops immediately. Other model errors and partial streamed output do not trigger that recovery.
- All usage obtained, including from summaries, failed attempts and cancellations, is accumulated onto the same side-question request. When a Provider returns no usage, only zero can be recorded; an estimate must never be passed off as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the summary of older question/answer pairs, the ordinals it covers, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, the number of pairs actually replayed, summary usage and the budget estimate.

A summary is only saved while the original request is still running and the clear version matches; clearing also clears the cache, and a late write never resurrects cleared data. A new snapshot never reuses the old snapshot's summary. The summary fields are saved with a v3 task archive; when restoring an older v3 the missing fields are filled with an empty object, and v1/v2 remain compatible.

POST accepts and returns the request first, with preparation and compaction running in the background without holding an admission lock or a database transaction. SSE/history show the preparing, consolidating question/answer pairs, compacting the copy and answering stages; a compaction failure is saved as that side-question request's terminal failure state. The frontend keeps the error and restores the draft of the failed question rather than covering the input box with a floating notification; history polling no longer wipes the submission error.

## Verification record

- Replaying 19/20/21/50 pairs, retaining conclusions older than 20 pairs, reusing the summary cache across a restart: automated, passing.
- Very long non-ASCII answers and code contexts, chunked request budgets, tool pairing, snapshot immutability, cache invalidation on a new snapshot: automated, passing.
- Summary failure/cancellation/truncation/oversize/tool returns, clear races, the call cap, exactly one overflow recovery, and no retry on a partial stream: automated, passing.
- Pagination against a dedicated PostgreSQL, restarts, v1/v2/v3 archives, archive and restore of the summary cache and budget metadata, an older v3 missing the new fields, and 20 parent sessions sharing four concurrency slots: passing.
- Go candidate service build, frontend TypeScript check, Biome check of the modified components, and a Next.js production build in a separate directory: passing.
- The built-in browser used a dedicated UI fixture to verify, at 1280x720 and 390x844, the consolidation stage, the summary scope hint, draft restoration after a failure, the absence of floating error notifications, no horizontal overflow and no console errors. The temporary fixture has been removed.
- A read-only replay of an existing local worker snapshot passed the new budget check; for example worker #3's 293085-character snapshot is no longer wrongly rejected by the local character count. No external model call was involved.
- A real conversation test that would have sent a private worker snapshot to Grok was refused by the automatic approval review and was not run, so it does not count as a pass.
- At 2026-09-11 00:37 the local backend was restarted at the user's request, reusing the original database, data directory and login configuration. The running file and the candidate binary have identical SHA-256, and `/api/health` returned healthy through both the backend and the frontend proxy.

Verification commands (using a dedicated test database only):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses a separate copy so the `.next` currently being previewed is not overwritten. The candidate service lives at `/private/tmp/artex-btw-budget-candidate`, has been copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
