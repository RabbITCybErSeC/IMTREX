---
name: api-recon
description: Invoke this skill when collecting a website's API endpoints.
---

# API Recon (frontend endpoint reconnaissance)

Under **authorization**, discover as completely as possible: the **backend APIs** (paths, methods, parameters, response bodies), the **frontend routes** and the **UI trigger points** (tabs, dialogs, table actions and so on).

---

## Boundaries and prohibitions (the agent must read this; violating it is out of scope)

This skill does **endpoint / parameter surface reconnaissance only**; it is not a vulnerability hunting or exploitation phase.

### Task boundary

| Area | Allowed | Forbidden |
|---|---|---|
| **Objective** | enumerate paths, methods, parameters, routes and UI trigger points | SQLi/XSS/authorization bypass/brute force/fuzzing, tampered-request attacks, destructive operations |
| **Authentication** | hook + stub/mock to bypass the **client-side** login gate | asking the user for or guessing credentials; submitting a real login form |
| **Runtime** | hook the endpoints without credentials and use mock responses to get the SPA into its logged-in shell | any flow that genuinely needs a real backend session to continue |

### Credential-free dynamic analysis (the Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** the login, permission, menu and other bootstrap endpoints;
2. Return a mock body for business query endpoints that is **structurally correct, carries a success business code, and may hold empty data**;
3. This lets the frontend render its logged-in pages with no backend or under 401, triggering more XHR/fetch/WebSocket calls;
4. **Empty data, blank tables and placeholder UI are all expected** -- do not switch to a real login or vulnerability testing because of them.

**In one sentence**: use mocks to prop the frontend routes and components open and **record only the outbound requests**; what the backend returns does not matter, what matters is **which endpoints the frontend still calls**.

### Hard process prohibitions

| Forbidden | Do this instead |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 is complete | run `OUTDIR/harvest_static.py` |
| hand-writing a script such as `extract_apis.py` to replace harvest | edit `OUTDIR/harvest_static.py` and rerun it |
| repeating the same grep/command after it has failed twice or more | change tactics: read tool_logs, edit harvest, check the reference |
| skipping gates A/B and running the original `scripts/` directly | copy them into OUTDIR and adapt them to the target |
| real usernames/passwords, OTP, OAuth or other authentication | stub/mock (see above) |
| skipping the stub "to get real data" and doing authorization-bypass or injection testing | record outbound only; that is the recon boundary |
| deleting, exporting sensitive data, bulk writes or other irreversible operations | the same applies to coverage clicks |
| claiming every page and endpoint was obtained without completing runtime + dynamic enumeration | see "Definition of done" or note the limitation |
| claiming every parameter is known without completing the parameter trigger matrix + diff | the Phase 3b matrix + the Phase 5 diff |
| inferring required/optional from a single runtime sample | diff several samples, or work backwards from validation rules/errors |

---

## The two-layer model + run modes

| Layer | Produces | Limits |
|---|---|---|
| **Static** (JS bundles) | every endpoint path, a draft route list, field candidates at the request-assembly sites | no HTTP methods; parameters need Phase 1b; misses URLs assembled at runtime |
| **Runtime** (a live session) | methods + bodies + responses + dynamic URLs + WS/SSE; multi-sample diffs complete the parameters | a page must actually render before it issues requests; one sample is not enough to decide required/optional |

| Run mode | Engine | Suited to |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | the API inventory, METHOD/params/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | clicking tabs/dialogs/tables for deeper feature coverage |
| **both** | depth first, then coverage | the most complete and the slowest |

**Parameter methodology** (there is no universal script): paths come from harvest/regex; parameters come from **anchor window expansion + the UI binding chain + multi-sample diffs + working backwards from errors** (grep recipes are in section J of [reference.md](reference.md)).

---

## Definition of done

Recon may only be declared complete when all of these hold:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt` and `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both additionally need **working hooks + the dynamic enumeration loop**
- [ ] **Inside the shell**: visiting a business path does not land on `/login` (mind hash routing)
- [ ] **Parameters**: coverage/both completed the parameter trigger matrix + `param_samples.json`; Phase 5 merged `params_merged.json`
- [ ] **Depth** (if a module page is blank): Phase 4 reconstructed the permission tree and reran until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5's outputs are complete (see the Phase 5 output table); `insert_assets` wrote the service and endpoint assets

---

## Scripts and gates

`scripts/` holds reference templates only; running the originals and treating the result as final is **forbidden**.

**The rule**: read first -> adapt to the target -> write into `OUTDIR` (such as `recon/`) -> record it in `CHANGES.md`; when they do not fit, rewrite per the methodology and borrow only the structure.

| Gate | When | Reference script -> OUTDIR copy | What usually needs changing |
|---|---|---|---|
| **A (static)** | after Phase 0, before running harvest/spider for the **first** time | `harvest_static.py` / `spider_mpa.py` | **the default regex runs as-is on most sites**; only when the manifest/dialect does not match do you change the endpoint regex, the webpack/Vite `publicPath`, or the MPA exclude/cookie |
| **B (runtime)** | after Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | the cookie/localStorage keys, the neutralized success values, the stubs, the login regex, the api prefix, hash/history |

**The mandatory SPA order** (not interchangeable; the phase numbering outranks "explore first, script later"):

| Step | Must | Forbidden |
|---|---|---|
| after Phase 0 completes | the next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read of the main entry `index-*.js` (usually >500KB) |
| gate A | copy the script -> make small adaptations -> **run it immediately** | extracting APIs by hand first and only then deciding whether to harvest |
| before Phase 1 completes | verify the output with `wc -l`; on a 404 fix harvest and retry | hand-written extract scripts; repeatedly grepping URLs that were never downloaded |
| from Phase 1b onwards | grep only `OUTDIR/js/*.js` | using the main bundle instead of harvest |

- OK: copy `harvest_static.py` -> (optionally) adjust the regex -> **run it immediately**
- NOT OK: curl the main bundle -> grep repeatedly -> write a temporary extract -> harvest only at the end
- **MPA**: after Phase 0 the next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Detail |
|---|---|
| large files | Read/grep of an `index-*.js` over 100KB into the context is **forbidden**; batch-process it with a script in OUTDIR |
| grep output | always `\| head -20` or `-m 5`; keep only a path summary in the conversation and never paste bundle fragments |
| verification | use `wc -l` and `ls \| wc -l`; do not Read a whole directory |
| exploratory regex | optional, at most once, and only on a chunk under 50KB or on HTML; the authoritative static pass is harvest |
| reference | recipes/templates/troubleshooting are in [reference.md](reference.md); do not inline the whole thing again |

---

## Execution roadmap

```
Phase 0 classification + OUTDIR
  -> gate A -> Phase 1 harvest (* run it immediately *)
  -> Phase 1b parameter reverse engineering
  -> Phase 2 the three authentication gates -> config.json
  -> gate B -> Phase 3 runtime + the parameter matrix
  -> Phase 4 the permission tree (when needed) -> rerun Phase 3
  -> Phase 5 merged report + insert_assets to bulk-insert every service and endpoint asset discovered; under no circumstances may a discovered asset be left out of the insert
```

Tick them in order; **the next phase must not start until the previous item is complete**.

1. [ ] **Phase 0**: initial SPA/MPA classification; create `OUTDIR` -> [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy the script -> harvest **immediately** -> verify with `wc -l` -> [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor window expansion + the binding layer -> `param_candidates.json` -> [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: the three authentication gates -> `config.json` -> [Phase 2](#phase-2--the-three-authentication-gates)
5. [ ] **Gate B**: adapt the runtime scripts -> [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm you are inside the shell; the parameter trigger matrix -> `param_samples.json`
7. [ ] **Phase 4** (if needed): the permission tree -> patch the stubs -> rerun Phase 3 -> [Phase 4](#phase-4--permission-tree-reconstruction)
8. [ ] **Phase 5**: merge the outputs + the report + `insert_assets` -> [Phase 5](#phase-5--merging-and-reporting)

---

## Phase 0 — classification

Fetch the entry HTML and **create `OUTDIR`** (do not edit `scripts/` inside the skill):

- **SPA**: an empty shell + `<div id=app>` + chunks -> Phases 1-5
- **MPA**: SSR + `<form>`, with no endpoint bundle -> after gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

It produces `forms.txt`, `links.txt` and `api_inline.txt`. For an SPA with forms around 0, switch to Phase 1.

---

## Phase 1 — static

Observe [scripts and gates](#scripts-and-gates) and [tool and output constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse the HTML scripts -> the webpack/Vite manifest -> download every lazy chunk -> produce `js/`, `api_static.txt`, `routes.txt` and `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- the chunk count vs the manifest: on a 404 fix harvest and retry rather than curling chunks one by one by hand
- too little in `api_static.txt` -> loosen the endpoint regex inside OUTDIR and rerun (see the reference)

### Phase 1b — parameter reverse engineering

The paths come from Phase 1; the parameter fields need their own recon. The grep rules are in [tool and output constraints](#tool-and-output-constraints).

**Done when**: for every important endpoint you can answer -- the field name, where it travels, the inferred type, whether it is required, a sample value and a confidence level.

#### 1b.0 — the transport shape

| Shape | Where the parameters are | Look at first, statically |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` next to the path anchor |
| GraphQL | `variables` | the gql template, `$page: Int` |
| classic form | urlencoded | `<form>`, `FormData` |
| file upload | multipart | `FormData.append` |
| path parameter | `/user/:id` | the route table + `useParams` / `$route.params` |
| encrypted/signed | wrapped in `sign`/`data` | hook the encryption function's arguments (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — anchor window expansion

Using a known path as the anchor, widen the window to find the object being assembled:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clues |
|---|---|
| an axios instance | `data` / `params` |
| a unified request helper | fields injected globally by an interceptor |
| an OpenAPI client | the generated method signature |
| React Query / SWR | the hook's second argument |
| a Vue composable | the composable's arguments |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, an embedded Swagger.

-> `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — the binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| a form submit | follow submit -> transform -> API |
| a table search | `getFieldsValue()` -> `params` |
| routing | `:id` / `?tab=` |
| an interceptor | a global `tenantId`, pagination, a signature |
| an enum select | `options` -> the API's enum values |

In DevTools, walk up the call stack from `fetch`/`XHR.send` to the assembling function.

#### 1b.3 — the three assembly questions (not the same as Phase 2's three authentication gates)

| Question | What it must answer |
|---|---|
| **assembly** | where the payload is built and any transform traces |
| **validation** | required, pattern, enum |
| **transport** | path / query / body / multipart / headers |

While at the interceptor gate (Phase 2), also read the globally injected fields (Authorization, `X-Tenant-Id`, the signature).

#### 1b.4 — handing over to Phase 3

The candidate fields come from the static/binding layer; **required/optional/conditional dependencies** need Phase 3's parameter matrix + diffs + Phase 5's working backwards from errors.

---

## Phase 2 — the three authentication gates

grep inside `OUTDIR/js/` (with `head`) and write the findings into `config.json` (recipes are in the reference):

| Gate | Question | Keywords |
|---|---|---|
| **the render gate** | how does it decide you are logged in? | `isLogin`, `getToken`, cookies/localStorage |
| **the interceptor gate** | what triggers the jump to `/login`? | `response_code`, `errno`, the axios interceptor |
| **the content gate** | where do the menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat a localStorage key name as a credential -- confirm it from the chunk/request chain.

**The exit = gate B**: land the conclusions in `config.json` and adapt `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API observation (optional)

Use `preload.js` inside OUTDIR to confirm the session key names, Authorization and nested API URLs:

| Setting | Produces |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | header observation |
| `extractUrlsFromResponse: true` | sub-APIs inside the responses |
| `observe.storageReads/cookieReads: true` | values to back-fill into config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — runtime

Gate B must already be passed; observe [boundaries and prohibitions](#boundaries-and-prohibitions-the-agent-must-read-this-violating-it-is-out-of-scope) and the credential-free mock strategy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (the template is in the reference).

### Hooks and stubs (shared by depth and coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 precise | stub auth/permission/bootstrap | get past the first-screen authentication |
| L2 negative correction | every JSON response | a not-logged-in code -> success |
| L3 catch-all | `/api` and similar not matched by L1 | an empty success body, to prop the UI open |

- **depth**: fake auth + `forward` rewriting the business code + `stubs`; walk `routes` (hash/history); produces `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or a userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; a business path does not bounce back to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage dynamic enumeration (mandatory)

1. Main navigation / sidebar -- click every item and wait 1-3s for the network
2. Tabs -- `role=tab`, `.ant-tabs-tab`
3. Tables -- view/edit/details on the first row
4. Toolbars -- export, filter, create (**avoid irreversible deletions**)
5. On entering each module -- merge the APIs/routes
6. SPA -- a controlled `pushState` for paths in `routes.txt` not yet covered (forbidden for an MPA)

**The parameter trigger matrix** (mandatory): record one of each operation type per module and **diff the samples**:

| Operation | The parameters it usually adds |
|---|---|
| the list's first screen | pagination + the default filters |
| clicking search | keyword, filter |
| advanced filtering | more optional fields |
| create/edit | the complete entity |
| bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Under a stub the outbound body/headers are still real** -- go by the request. Record into `scan_raw.json`, `param_samples.json` and `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + a document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — permission tree reconstruction

**Trigger**: module pages are blank / every route issues only bootstrap calls (locale and the like) -> the content gate was not passed.

| Symptom | Meaning |
|---|---|
| you are inside the shell | the render gate and interceptor gate are passed |
| sidebar items missing / clicks do nothing | the stub shape or the permission codes are incomplete |
| every route issues the same, very few APIs | `v-if permission` did not pass |
| `routes.txt` has far fewer than the bundle | it must be completed from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

A typical chain: `role_permissions` (flat codes) + `permissions/all` (a tree) -> `getResultTree` -> `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub checks: the outer `response_code` matches the interceptor gate; the flat codes line up with the tree; `routes` covers every link in `route_map`.

After updating `config.json`, **rerun Phase 3**. For a large SPA you can tune `waitUntil`, `routeTimeout` and `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — merging and reporting

### Output table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | the static bundles and paths |
| `param_candidates.json` | 1b | static parameter field candidates |
| `config.json` | 2 | the three gates + the runtime configuration |
| `runtime_api.json` | 3a | the detailed depth recording (including WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | the samples, the click log and the detail |
| `route_map.json` and others | 4 | the permission tree intermediates (if it ran) |
| `params_merged.json` | 5 | the merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | routes, APIs, params, feature points and limitations |
| **insert_assets** | 5 | write every service and endpoint asset into the asset database |

### 5b — merging the parameters

Diff from `param_samples.json`; **there is no universal merge script**. The confidence rules are in reference J7 (high/medium/low/not yet triggered).

### 5c — working backwards from errors

Within the authorized scope you may send an incomplete request to read a 400 (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors and so on. Watch out for the `data` wrapper, `variables` and the pre-encryption `bizData`.

The report must state: the runtimeMode, the static/runtime API counts, the parameter confidence levels, the modules not covered, and a summary of `CHANGES.md` relative to the reference scripts.

A suggested structure for `site_map.json`:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

More fields and grep recipes are in [reference.md](reference.md).

---

## General notes

- **Framework-agnostic**: webpack/Vite/Angular lazy loading all work the same way
- **Transports**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetches can be recorded; RSC/Server Actions are not fully enumerable
- **Blind spots**: JSVMP, WASM, strict HMAC/mTLS validation -> stay static and note the limitation
- **Parameter blind spots**: conditional coupling, hidden params, WASM-side assembly -> mark them "not yet triggered" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate the endpoints

---

## Additional resources

- Grep recipes, the `config.json` template, troubleshooting, hooks, parameter reverse engineering section J and the site_map template: **[reference.md](reference.md)**
- The reference script paths are in the [scripts and gates](#scripts-and-gates) table
