# `/btw` validation record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

A dedicated PostgreSQL test database and data directory were used; real model credentials were injected only into the isolated test environment, were never written into the code or this record, and the product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The actual model conversations, returned objects, engineering assertions and the raw Qwen review text are stored in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, deep copy of tool arguments | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite; complete replies and terminal states are published; half-finished replies excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Identity of the actual model pool member | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-pair replay, budget trimming and the over-limit error | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main and side questions in parallel, bidirectional cancellation isolation | Pass | A blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, usage already accrued on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| A real norma ChatAgent + the local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subcases |
| Persistence, pagination, idempotency, partial answers surviving a restart | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs late-write races, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / worker archive and restore, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| All three parent endpoints, authentication, resource ownership, worker logical deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Side questions work while the main session is busy, independent SSE reconnect / disconnect, cancellation, clearing | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 concurrent request per parent session / 4 globally | Pass | Two `TestSideHTTP...` cases |
| Snapshot persisted before submission, follow-up after a restart, an old session cannot fabricate a snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Refusing to continue after the cached configuration is deleted or its model changes | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancelling before archiving and waiting for the final answer and usage to persist | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Usage recorded exactly once, and attributed to the side question, when a streaming consumer cancels early | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| A worker restored after a restart / a deadline run context keeps publishing new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check of the relevant packages | Pass | The commands below |
| TypeScript and the production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on the new frontend modules | Pass | `biome check`, 3 new modules |

With `ARTEX_PG_DSN` pointed at a separate throwaway database, the automated checks can be reproduced (do not point it at a production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not entirely green: two pre-existing tests in the `server` package fail while cleaning up their temporary directory, both reporting `TempDir RemoveAll ... directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting the source from the unmodified baseline above and re-running the `server` package in the same isolated environment, both cleanup failures reproduce. The baseline run additionally showed an assertion failure on the goal node count in `TestCoreTaskLifecyclePG`; the final modified `server` regression does not have that failure. The other packages pass, and the side-question cases and race checks of this change pass. Baseline problems were not marked as passing for this change, and no existing assertion was modified to hide a problem.

The Next.js build emits the pre-existing multiple-lockfile / workspace root inference warning; the build completes and every page is generated successfully.

## Browser checks

The Codex in-app browser was used, connected to a separate local Go service and the Next.js dev server. The following manual automation was performed on desktop and at 390 x 844, checking screenshots and browser logs:

- Typing `/btw` while an ordinary chat was running showed the main content and the side question at the same time; the desktop side panel behaved correctly.
- Consecutive follow-up questions; stopping a side question kept the part already generated; the main flow continued.
- The request continued while the panel was closed, and the completed answer was restored on reopening; after a page refresh an empty `/btw` restored the history.
- Input, buttons, history and closing worked in the narrow-screen drawer, with no horizontal overflow.
- Clearing used a confirmation dialog; afterwards the history was gone while the main transcript and the snapshot remained.
- Questions were asked in a task's MainAgent and in two workers and switched between; the agent labels and histories never crossed over.
- A blocking local model fixture kept a worker running; submitting `/btw` from the worker's main input box and then stopping the side question left the worker still showing a live run with its own pause button, and the side question kept its partial answer.
- The browser error / warning log was empty.

A controllable fixture was used to verify concurrency timing precisely without depending on a real model's output speed. During debugging, two worker-runtime checks failed to create a real concurrency window (the task had already finished / the answer ended early); the fixture was corrected and they were redone and passed. Those initial attempts are not recorded as valid passes.

## Real model conversations

`grok-4.6` was probed first, over the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, the model name `grok-4.6` and `READY` in 2.82 seconds. The first choice was available, so the Tingly `glm` and Zhipu `glm-5.3` fallback chains were not used; neither fallback service was validated here.

| Scenario | Actual result |
| --- | --- |
| Asking about assets, goals and a marker while the main session was running | Returned `redhaze.top`, the homepage read and goal summary, and `BTW-REAL-0910`; the side question completed in 16.97 seconds |
| Asking what the tools showed after the main session finished reading the homepage | Correctly cited WebFetch 200, the curl redirects 301 -> 302 -> 200 and the page title; 7.24 seconds |
| A side question asking Bash to create a test file | Execution refused, the target file was not created; 7.74 seconds |
| A side question after completion does not change the main context | The main transcript SHA-256 and the main activity log were unchanged; the side question executed 0 tools |
| A follow-up after genuinely stopping / restarting the Go service | The 3 previous side questions were preserved, and the assets, marker and title were answered straight from the persisted snapshot without re-running the main agent |
| A new session using the non-streaming Grok configuration | Correctly answered the assets and `ATOMIC-0910`; usage returned and saved: input 11734, output 138, cache_read 11520 |

The main session for the asset case used WebFetch and Bash/curl to read a public homepage; the landing page was `https://id.redhaze.top/home` with the title "RedHaze Group - global conglomerate portal". Bash stored the response in a local test file; nothing was written to the remote host. That fact is verified separately from "the side question executed no tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's streaming Grok responses return no usage. This was verified separately by sending `stream_options.include_usage=true` directly: HTTP 200, 12 data frames, 0 usage frames. So the 0 in the streaming tests means the endpoint provided no usage data and must not be read as "nothing was billed". Non-streaming usage, and the fixture's failure / cancellation usage, are saved correctly.

## Qwen review

The reviewing model was `qwen-flash` over the OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. It was given the first three real side-question conversations, the main session's tool evidence and the engineering assertions; it returned `verdict: accept` with `concerns: []`, judging the answers consistent with the asset, marker and page-read evidence and the side question's tool refusal consistent with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not cover the service restart and non-streaming tests added afterwards. Qwen's generalization about "no writes" was too broad: the main session's curl did create a local temporary response file, as recorded explicitly above. Concurrency, zero tool executions and transcript isolation are judged by the engineering assertions; the model review only helps assess answer quality.
