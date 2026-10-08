# CJK-to-English translation plan

Status: active. Baseline measured with `tools/find_cjk.py` on `main` (`a5047c3`).

## 1. Objective and definition of done

Drive the repository to zero unallowlisted CJK (Chinese/Japanese) characters in code,
comments, UI strings, mock data, documentation, and file names.

Every tolerated occurrence must be one of:

1. **Allowlisted** in `tools/cjk-allowlist.json` with a written reason, or
2. **Inline-marked** on the line itself with the `cjk-allow` marker (used only where
   the line *is* the reason, e.g. glossary rows in this document).

Definition of done:

- `python3 tools/find_cjk.py --check` exits 0.
- CI enforces the same command on every pull request.
- The README "About this fork" section no longer carries the translation-review caveat.

## 2. Tooling (deterministic detection)

`tools/find_cjk.py` is the single source of truth. Stdlib-only, Python 3.8+, deterministic:
the file list comes from `git ls-files`, output is sorted, and identical trees produce
identical reports. No manual scanning is needed at any point.

```bash
python3 tools/find_cjk.py                      # full human-readable report
python3 tools/find_cjk.py --stats              # inventory summary
python3 tools/find_cjk.py --check              # CI gate: exit 1 while work remains
python3 tools/find_cjk.py --json               # machine-readable inventory
python3 tools/find_cjk.py --path web/src       # limit to a subtree
python3 tools/find_cjk.py --show-allowed       # also list allowlisted lines
```

What it detects:

- Han ideographs (incl. Ext A/B), kana, Bopomofo, CJK punctuation, fullwidth forms.
- CJK in file names (reported as `PATH NAME` violations).
- Invalid UTF-8 (possible GBK/Shift-JIS text), reported as `NON-UTF8`.
- Binary files are skipped (NUL-byte sniff); only tracked files are scanned.

Exit codes: `--check` fails (1) while violations remain; all other modes exit 0.
During development, add `--include-untracked` to also scan not-yet-committed files.

Policy mechanics:

- `tools/cjk-allowlist.json` entries have `path` (glob, `**` crosses directories),
  optional `line` (regex), and a mandatory `reason`. Regexes use `\uXXXX` escapes so the
  allowlist file itself stays ASCII-clean; a bare `path` entry also tolerates a CJK file
  name.
- Inline marker: a line containing `cjk-allow` is reported as allowed, never as a violation.
- The tool and the allowlist have no self-exception: both are scanned like any other file.

Recommended CI gate (add once the baseline is at zero, or immediately in report-only form):

```yaml
name: cjk-check
on: [pull_request]
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Scan for CJK
        run: python3 tools/find_cjk.py --check
```

## 3. Baseline inventory (after the seeded allowlist)

`violations: 2697 lines in 49 files | path names: 1 | allowed: 85`

| Area | Lines | Files | Notes |
| --- | ---: | ---: | --- |
| web/src/components | 695 | 25 | React components, mixed comments + UI strings |
| web/src/lib/mock | 593 | 3 | Demo mock data + mock backend |
| web/src/app | 472 | 7 | Settings, skills, notify, tools pages |
| CHANGELOG.md | 454 | 1 | Historical upstream changelog (untouched by the fork) |
| web/src/lib (rest) | 413 | 12 | types.ts, api.ts, status.ts, helpers |
| docs | 43 | 1 | Chinese-named reference doc (also a PATH NAME violation) |
| web/src (rest) | 27 | 7 | sidebar, proxy.ts, config, hooks |

No Go, SQL, shell, or skills files remain: every Go/SQL hit is in the deliberate keep-list
below and is already allowlisted.

## 4. Keep-list (must not be translated)

These are already allowlisted; do not "fix" them. Each entry in
`tools/cjk-allowlist.json` carries the full reason.

| Category | Where | Why it stays |
| --- | --- | --- |
| Legacy `[model]` verdict prefix | db/intercept.go, db/schema.sql, db/task_archives_restore.go, web approval-records.tsx, mock handler | Pre-translation database rows carry the CJK prefix; it is matched alongside the new ASCII prefix so historical records still classify |
| ICP filing detection and fixtures | db/company_scope.go, web company-scope.ts, test fixtures | The CJK keyword and fullwidth punctuation are the data being matched; real ICP numbers contain them |
| Quota-exhausted markers | agent/provider.go, two test files | Chinese LLM gateways return these strings in error text; detection breaks if translated |
| Fullwidth comma in `split()` | notify channel-fields.ts, notify page.tsx | Accepts Chinese commas in operator input; functional data |
| Login-gate detection | skills preload.js, api-recon reference.md | Must match CJK wording on target pages |
| Fixture files proving non-ASCII handling | server/skill_upload_test.go, chat-mentions.test.mjs, company_scope_test.go, auth_test.go | The fixtures exist to prove CJK/GBK handling; translating them destroys their purpose |

Rule: a new allowlist entry is only acceptable with (a) a precise `line` regex where
possible, (b) a written reason, and (c) confirmation that the string is compared against
data, not prose. "Easier than translating" is not a reason.

## 5. Translation taxonomy and priorities

- **P0 - user-visible UI** (web/src/app, web/src/components): strings the operator reads.
- **P1 - frontend helpers** (web/src/lib except mock): comments plus a few UI strings.
- **P1 - demo mock data** (web/src/lib/mock): shipped to the Vercel demo.
- **P2 - CHANGELOG.md**: historical record; no code depends on it.
- **P3 - docs**: one Chinese-named file; decide delete vs keep.

## 6. Guardrails (what to check before changing a string)

The fork's history shows the real traps; check for them in every batch:

1. **Matched strings.** Before translating any literal, search for its other side:
   `grep -rn '<literal>' --include='*.go' --include='*.ts' --include='*.tsx'`.
   If it is compared, written to the DB, or matched against external data, keep it or add
   both variants (the `[model]`/legacy pattern).
2. **Format strings.** Translation reorders `%d`/`%s` arguments; six files needed argument
   swaps in the existing history (e.g. notify renderers). After editing, run the package
   tests; `go vet` catches arity but not order.
3. **Regexes and character classes.** A CJK character inside a class may be functional
   (fullwidth comma, CJK keywords). Keep and allowlist; do not translate.
4. **Protocol and schema identifiers.** Tool names, JSON keys, SQL identifiers, status
   enums stay ASCII (they already are); never localize them.
5. **Prompts.** Translate prose but keep structure and policy semantics; run
   `go test ./agent/... ./intercept/...` after touching prompts.
6. **Fixtures.** If a test's purpose is CJK handling, it stays (allowlist). If it merely
   used Chinese as convenient text, translate it.
7. **Layout.** Longer English strings can overflow dense UI; eyeball the affected page
   after a component batch.

## 7. Glossary (established vocabulary - reuse it)

Use the terms already established by the merged translation; do not invent synonyms.

| Source | Use | Note |
| --- | --- | --- |
| 漏洞 | finding | <!-- cjk-allow: glossary --> |
| 资产 | asset | <!-- cjk-allow: glossary --> |
| 任务 | task | <!-- cjk-allow: glossary --> |
| 探索链 | exploration chain | <!-- cjk-allow: glossary --> |
| 意图 | intent | <!-- cjk-allow: glossary --> |
| 事实 | fact | <!-- cjk-allow: glossary --> |
| 提示 | hint | <!-- cjk-allow: glossary --> |
| 摘要 / 冷节点压缩 | digest / cold digest | <!-- cjk-allow: glossary --> |
| 收尾 | wrap-up | <!-- cjk-allow: glossary --> |
| 拦截 | intercept / interception | <!-- cjk-allow: glossary --> |
| 审批 | approval | <!-- cjk-allow: glossary --> |
| 流量 | traffic | <!-- cjk-allow: glossary --> |
| 证据 | evidence | <!-- cjk-allow: glossary --> |
| 复测 | retest | <!-- cjk-allow: glossary --> |
| 报告 | report | <!-- cjk-allow: glossary --> |
| 工作 Agent / 规划器 / 主 Agent | worker / planner / main agent | <!-- cjk-allow: glossary --> |
| 会话 | session | <!-- cjk-allow: glossary --> |
| 触发 | trigger | <!-- cjk-allow: glossary --> |
| 归属 | attribution | <!-- cjk-allow: glossary --> |
| 备案 | ICP filing | <!-- cjk-allow: glossary --> |
| 严重度 | severity | <!-- cjk-allow: glossary --> |
| 推送 / 渠道 | notification / channel | <!-- cjk-allow: glossary --> |
| 掩码 | masked value | <!-- cjk-allow: glossary --> |
| 限流 | rate limit | <!-- cjk-allow: glossary --> |
| 租约 | lease | <!-- cjk-allow: glossary --> |
| 熔断 / 冷却 | trip / cooldown | <!-- cjk-allow: glossary --> |
| 重试 / 退避 | retry / backoff | <!-- cjk-allow: glossary --> |
| 账本 / 计量 | ledger / metering | <!-- cjk-allow: glossary --> |
| 假删除 / 真删除 | soft delete / hard delete | <!-- cjk-allow: glossary --> |
| 暂存 / 换装 / 回滚 | staged / swap / rollback | <!-- cjk-allow: glossary --> |
| 出口代理 | egress proxy | <!-- cjk-allow: glossary --> |
| 上下文窗口 / 输出上限 | context window / output cap | <!-- cjk-allow: glossary --> |

## 8. Phases

Each phase is a series of small batches (one file or a few tightly related files per
commit). Acceptance for every phase: `python3 tools/find_cjk.py --path <phase paths>`
reports zero violations, builds/tests pass, and no new allowlist entry lacks a reason.

### Phase 0 - Tooling (done)

- `tools/find_cjk.py`, `tools/cjk-allowlist.json`, this plan, baseline captured.

### Phase 1 - web/src/components (695 lines, 25 files)

Suggested batch order (largest first): agent-editor (181), approval-records (133),
asset-dsl-search (79), task-template-controls (42), side-question-workspace (33),
transcript (30), finding-traffic-panel (24), traffic-picker-dialog (24),
finding-retest-panel (19), task-llm-profile-chain (17), asset-intercept-rules-editor (16),
mention-textarea (15), link-traffic-dialog (12), finding-retest-dialog (11),
traffic-evidence-viewer (11), approval-execution-focus (9), copy-button (7),
http-code-block (7), scope-text-editor (7), ui/dialog (6), table-pagination (5),
todo-popover (5), ui/sortable-head (2).

Watch for: strings that mirror backend enums (status labels in approval records are
matched against `decision` values), and the legacy-prefix lines already allowlisted.

### Phase 2 - web/src/app pages (472 lines, 7 files)

settings page (121), skills page (116), notify page (98) + stat-tile (4), tools page (81),
settings update-card (52).

### Phase 3 - web/src/lib except mock (413 lines, 12 files)

types.ts (181), api.ts (107), status.ts (53), utils.ts (15), chat-send-mode (13),
task-assets (13), company-scope (12), chat-mentions (11), auth (4), side-questions (3),
preferences-storage (1).

`types.ts` and `api.ts` are almost entirely comments; the few user-visible strings are in
`status.ts` and error helpers.

### Phase 4 - web/src/lib/mock (593 lines, 3 files)

mock/data.ts (412), mock/handler.ts (179), mock/enabled.ts (2).

This is demo data for the Vercel build. Translate the prose; keep the legacy `[model]`
prefix lines (already allowlisted) and any value that mimics a real-world fixture
(ICP example).

### Phase 5 - web/src rest (27 lines, 7 files)

navigation sidebar-items (19), proxy.ts (4), config/app-config (2), hooks (2),
and the remaining small files listed by the scanner.

### Phase 6 - CHANGELOG.md (454 lines)

No code depends on it; lowest risk, lowest urgency. Two options:

- **Option A (default): translate.** It is the public changelog on GitHub; the fork's
  README already promises English. Work top-to-bottom in version sections, one release
  block per commit.
- **Option B: keep as historical.** Add one whole-file allowlist entry with the reason
  "upstream historical changelog retained in the original language". This keeps the gate
  green without editing 454 lines.

### Phase 7 - docs decision (43 lines + 1 path violation)

`docs/漏洞流量证据.md` <!-- cjk-allow: file under decision --> duplicates the English
`docs/vulnerability-traffic-evidence.md` added by the fork. Options:

- **Default: delete the Chinese copy.** Upstream keeps the canonical original; the English
  twin covers this repo. This also clears the PATH NAME violation.
- Alternative: keep it as a reference with a path allowlist entry whose reason says so.

### Phase 8 - close-out

1. Audit the allowlist: every entry still needed, precise, and reasoned.
2. Add the CI gate workflow (section 2).
3. Update the README "About this fork" section: drop the review caveat, note the gate.
4. Final `--check` on a clean checkout.

## 9. Per-batch workflow

```bash
# 1. See the work
python3 tools/find_cjk.py --path web/src/components/agent-editor.tsx

# 2. Translate comments and UI strings; before changing any literal that might be
#    matched, grep for its other side first (section 6).

# 3. Re-scan the file
python3 tools/find_cjk.py --path web/src/components/agent-editor.tsx

# 4. Verify
cd web && npm run check && npx tsc --noEmit        # frontend batch
go build ./... && go test ./...                    # Go batch (none currently pending)

# 5. Commit (one logical batch per commit)
git add -A && git commit -m "i18n(web): translate agent editor"

# 6. Global gate before pushing
python3 tools/find_cjk.py --check
```

## 10. Verification matrix

| Layer | Command | When |
| --- | --- | --- |
| Detector | `python3 tools/find_cjk.py --check` | every batch |
| Frontend lint | `cd web && npm run check` | web batches |
| Frontend types | `cd web && npx tsc --noEmit` | web batches |
| Frontend build | `cd web && npm run build:static` | end of each phase |
| Go build/tests | `go build ./... && go test ./...` | any Go/skills batch |
| Visual check | affected page in `npm run dev` | component/page phases |

## 11. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Translating a matched string breaks behavior or legacy data | Section 6 rule 1; keep-list; tests per batch |
| Format-string argument order drift | Run tests after each edit; the existing history shows six such swaps |
| Prompt regression after translating agent text | Keep structure; run agent/intercept tests; prompts are already English in the fork |
| Test fixtures lose their purpose | Fixtures proving CJK handling are allowlisted, not translated |
| UI overflow with longer English | Visual check per component batch |
| Scanner false positives | Inline `cjk-allow` marker or allowlist entry with reason; never weaken the ranges without review |
| Allowlist rot | Phase 8 audit; entries require reasons and should use precise line regexes |

## 12. Appendix - full remaining work list

The 49 files with counts (as of the baseline): CHANGELOG.md 454; web/src/lib/mock/data.ts
412; agent-editor 181; types.ts 181; mock/handler 179; approval-records 133; settings page
121; skills page 116; api.ts 107; notify page 98; tools page 81; asset-dsl-search 79;
status.ts 53; update-card 52; the docs file 43; task-template-controls 42;
side-question-workspace 33; transcript 30; finding-traffic-panel 24; traffic-picker-dialog
24; finding-retest-panel 19; sidebar-items 19; task-llm-profile-chain 17;
asset-intercept-rules-editor 16; mention-textarea 15; utils.ts 15; chat-send-mode 13;
task-assets 13; link-traffic-dialog 12; company-scope 12; finding-retest-dialog 11;
traffic-evidence-viewer 11; chat-mentions 11; approval-execution-focus 9; copy-button 7;
http-code-block 7; scope-text-editor 7; ui/dialog 6; table-pagination 5; todo-popover 5;
notify stat-tile 4; auth.ts 4; proxy.ts 4; side-questions 3; ui/sortable-head 2;
app-config 2; use-side-questions 2; mock/enabled 2; preferences-storage 1.

Regenerate any time with `python3 tools/find_cjk.py --stats`.
