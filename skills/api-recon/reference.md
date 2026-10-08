# api-recon — reference manual

Grep recipes, the `config.json` template and troubleshooting. Every grep runs against the `js/` directory. When a bundle is one long line, run `js-beautify` or `sed 's/}/}\n/g'` first, though a raw grep with a context window is usually enough.

## About the scripts

Every file in `scripts/` is a **reference template** and must be adapted to the target site before it is run. Typical changes:

| Script | What usually needs adjusting |
|---|---|
| `harvest_static.py` | the endpoint regex, webpack/Vite manifest parsing, the micro-frontend publicPath, retries/concurrency |
| `runtime_harvest.js` | the neutralize field names and success values, the stub matching rules and body structure, where routes come from, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, the L1 stubs, `neutralize.fields`, `apiPattern`, whether L3 is enabled, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` for destructive links, cookies, depth/max, same-origin filtering |
| `extract_route_map.py` | the `routeMap` / `routeLink` regex, the KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, the `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, the stub's outer field names |
| `config.json` | the single entry point for all the site-specific parameters above |

Put the adapted files in the task working directory (such as `recon/`) and state in the report exactly how they differ from the reference scripts.

---

## A. Reversing the three gates

### A1. The render gate — "how does it decide you are logged in?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Find the chain `isLogin = f(getUser())` -> `getUser = decode(storage.read(KEY))` and determine the **storage key**, the **container** (cookie vs localStorage) and the **encoding**:

| Encoding | How to forge it in config |
|---|---|
| a plain string / `"1"` / a token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | an unsigned / `alg:none` JWT, or one signed with a key found in the bundle |
| encrypted (SM2/AES/RSA) | look for a hard-coded key; the render gate can be forged when it only needs a decodable blob, otherwise fall back to static |

-> write it into `cookies` / `localStorage`.

### A2. The interceptor gate — "what triggers the jump to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(not\s*logged\s*in|please\s*log\s*in\s*again|session\s*expired|unauthorized|login\s*invalid|authoriz|token.{0,10}invalid|未登录|请重新登录|登录已过期|登录失效|授权).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: the **field name**, the **success value** (usually `0` or `200`) and the **failure value that triggers the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

The Chinese alternatives in that pattern are deliberate: they are the literal strings a Chinese
enterprise frontend puts in its bundle, so dropping them would make the recipe miss the gate on those targets.

-> write it into `neutralize.fields` + `neutralize.success`.

### A3. The content gate — "where do the menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise back offices):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| `userRouteAuth` inside the bundle | `{ CODE: { url, name? } }` | code -> frontend path |
| `routeMap` inside the bundle | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Forging by hand** (a small site): build a permissive payload -> `stubs`.

**Full permission tree reconstruction** (a large site where the sidebar/submodules are still blank): see **section I**.

---

## B. The config.json template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefixes: `b64json:` -> base64(JSON); `json:` -> raw JSON; no prefix -> a literal
- `forward: true` forwards the real request and rewrites the code field; `false` is a fully offline stub
- `mockTier`: which layers preload enables in coverage mode, such as `L1+L2` or `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging the menu, the harness appends `<a href>` entries automatically
- `captureResponses` / `recordWs` only take effect in depth mode
- `waitUntil`: use `domcontentloaded` for a large SPA to avoid `networkidle2` hanging
- `routeTimeout`: the `page.goto` timeout per route (milliseconds)
- `proxy`: Puppeteer's `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` also work

### B1. The dual-stub template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

The outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate, and `permissions` must cover every leaf code in the tree.

---

## C. coverage mode: the preload configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before injecting through CDP:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* the same as config.json's stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and the pathname is stable.

Export the recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime hook capabilities

The browser hooks built into preload (coverage) and runtime_harvest (depth), and what they cover:

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| hook fetch / XHR.open | record the request URL/method | yes: `recordDetail` + `__API_RECON_LOG__` |
| hook XHR.setRequestHeader | discover Authorization and other headers | yes: `observe.xhrHeaders` |
| hook localStorage/cookie reads | confirm the session key names | optional: `observe.storageReads/cookieReads` |
| read the Vue routes | complete frontendRoutes | yes: `__API_RECON_ROUTES__` (the routes already loaded) |
| neutralize Vue route guards / block the login redirect | prop modules open so they trigger APIs | yes: `neutralizeVueRouter` + native redirect neutralization |
| read the React routes | complete the routes | partial: static + clicking; no dedicated hook |
| block page navigation (the login path) | stay on the page to analyse it | partial: only the login path is blocked, so business navigation is not |
| hook the crypto library (CryptoJS/SM and so on) | encrypted parameters -> the plaintext API body | no: hook the encryption function's arguments by hand and write the conclusion into config |
| anti-debugging bypass | without it, runtime records no APIs | no: handle it by hand; static still works |

---

## E. Endpoint extraction regexes (when static finds too little)

Loosen it in `harvest_static.py`'s `extract_endpoints`, or do it by hand:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause -> what to do |
|---|---|
| very few static APIs | the endpoint dialect does not match -> loosen the regex (section D) |
| far fewer chunks than the manifest | CSS-only or undeployed chunks; the 404s were already retried |
| runtime still shows the login page | the render gate is wrong -> recheck A1: the key name, the container, the encoding, the domain |
| inside the shell but the modules are blank | the content gate -> forge the menu (A3); the `routes` paths may be wrong |
| only bootstrap/locale per route | the permission codes are incomplete -> reconstruct the permission tree (section I); check the dual stub of `role_permissions` + `permissions/all` |
| sidebar items exist but the subpages are blank | the tree is missing an intermediate node, or a code does not match `userRouteAuth` |
| every API redirects to login | the interceptor gate -> confirm `neutralize`; a nested field needs the walk logic extended |
| 0 WS frames | it only subscribes after user interaction; increase `perRouteMs` |
| empty response bodies | only `forward: true` yields real responses |
| Chromium is missing | install chromium or set `config.chromium` / `CHROMIUM` |
| plenty of mocks but it still returns to login | the hook is too late or the `location.href` setter is missing -> document-start + preload |
| every list is empty | an L3 empty array is normal; keep clicking tabs/settings/details |
| a Redux action mistaken for a route | filter out internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload is not at document-start -> change when it is injected; or clear the guards by hand when `neutralizeVueRouter: false` |
| a URL appears in a response but not in the log | enable `extractUrlsFromResponse`; or extract it by hand from `__API_RECON_DETAIL__` |
| the Authorization header name is unknown | enable `observe.xhrHeaders` or inspect the request headers in DevTools |
| runtime is extremely slow / times out | switch to `waitUntil: domcontentloaded`; lower `routeTimeout`; do not use `networkidle2` |
| the proxy connection fails | check `proxy` / the environment variables; Puppeteer and curl must use the same proxy port |

---

## G. Hardened targets

When the server validates the session step by step (an unforgeable signed cookie, a server-rendered menu that cannot be stubbed), runtime gets stuck at the shell. Expected behaviour:

- **Static is enough for endpoint enumeration** -- the module paths are in the code
- If authorization allows, run the same harness with a **real session**: `forward: true`, no neutralization needed, capturing real methods/params/responses

---

## H. Single-task checklist

1. Confirm the authorized scope
2. **Read** `scripts/harvest_static.py` -> adapt it to the target -> run it -> review `api_static.txt` and `routes.txt`
3. **Phase 1b**: path anchor window expansion + the binding layer -> `param_candidates.json` (section J)
4. Reverse A1/A2/A3 -> write the site-specific `config.json`
5. **Read and adapt** `runtime_harvest.js` / `preload.js` before running them
6. `runtimeMode=depth`: `npm install` -> run the adapted harvest script
7. `runtimeMode=coverage/both`: inject the adapted preload at document-start -> browser MCP dynamic enumeration + **the parameter trigger matrix**
8. Modules do not render -> **section I permission tree reconstruction** -> patch the stubs -> rerun
9. Multi-sample parameter diffs + working backwards from errors -> `params_merged.json`
10. Merge -> `site_map.json` + `api_merged.txt`, honestly noting the coverage, the gaps and how the scripts were changed

---

## I. Permission tree reconstruction (Phase 4 in depth)

Use it when forging a simple `menus: [{ path, show: true }]` has no effect and submodules still do not mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names** and the **consuming chunk file name**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If you see `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep by hand:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

What the script does:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including the webpack alias `He=o.DASHBOARD`)
2. Resolve aliases to real paths with `route_map.json`
3. Infer the parent from the code prefix (`MONITOR_ALERT` -> `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json` and `role_permissions_stub.json`
5. With `--config`, write the `stubs` and the extended `routes` into `config.json` automatically

**Adapt to the target** (at the top of the script):
- `DEFAULT_ROOTS`: the list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: the `PREFIX_` -> parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with no prefix relationship

### I4. Check the stub is consistent

```bash
# the number of permissions should be about the number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover every link in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare the number of entries in runtime_api.json before and after forging; check whether module APIs appear for /attack, /asset and so on
```

| Before forging | After forging (success) |
|---|---|
| the same 3-5 bootstrap calls per route | different routes trigger different module APIs |
| only `/api/locale/language` | `/api/web/...` module endpoints appear |
| `routes.txt` has single-digit routes | `routes` has 80-110+ from route_map |

### I6. When it still fails

- **coverage mode**: click the sidebar + tabs; permission gating may only request after an interaction
- **stub fields**: compare the nesting of the real API (curl + a real session) with the stub
- **extra guards**: grep for `hasPermission|checkRole|func.` and other button-level checks, and extend `role_permissions.permissions`
- **static fallback**: the module API paths are still in `api_static.txt` and runtime only adds the METHOD/body; keep the parameters in `param_candidates.json` + the samples already recorded

---

## J. Parameter reverse engineering (Phase 1b / 5b / 5c)

**A methodology, not a universal script.** Find paths with regexes; find parameters with anchor window expansion + the UI binding chain + multi-sample diffs + working backwards from errors.

### J1. Anchor window expansion — find the assembled object from the path

```bash
# anchored on a path already known from Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layers and transport shapes

```bash
# axios / a unified request helper
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. The validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. The binding layer — form -> API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Filling the gap at runtime: DevTools -> Network -> the request -> **Initiator** (the call stack), walking up from `fetch`/`send` to the assembling function.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields from ciphertext** -- hook the encryption function's **arguments** and record the plaintext payload before encryption; write the conclusion into `config.json` / `param_candidates.json`.

### J6. The parameter trigger matrix (mandatory in Phase 3)

Record one of each operation per module and diff the request body/query:

| Operation | What to look at |
|---|---|
| the list's first screen | the pagination defaults |
| search | keyword, filters |
| advanced filtering | optional fields |
| create/edit | the complete entity |
| bulk/export | `ids[]`, `exportType` |
| sort/paginate | `sortField`, `order` |

Produces `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **high** | a static callsite + two or more consistent runtime samples |
| **medium** | static only, or a single runtime occurrence |
| **low** | inferred from a response/error without a second verification |
| **not yet triggered** | the field is known statically but the UI/permissions never reached it |

### J8. Quick recipes by scenario

| Scenario | Order |
|---|---|
| a REST list page | J1 the assembled object -> J6 four diffs -> J3 rules |
| a create/edit form | J3 the Form name -> J4 the submit chain -> submit at runtime and deliberately leave a field empty to see the 400 |
| GraphQL | J2 the variables declaration -> record the variables of each operation at runtime |
| an encrypted body | J5 hook the arguments -> the pre-encryption fields are the real params |

### J9. Mapping onto the api-recon phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 the interceptor | globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 the trigger matrix + `param_samples.json` |
| Phase 4 the permission tree | different modules have different forms -> only enough permissions trigger the full field set |
| Phase 5 merging | `params_merged.json` + confidence; never decide "required" from one sample |

### J10. Troubleshooting

| Symptom | What to do |
|---|---|
| a field name exists statically but never appears at runtime | mark it "not yet triggered"; complete the permission tree / click advanced filtering / try each option of a linked select |
| the same path with different body shapes | normal -- record them separately by `action` and do not force one schema |
| the stub response is fake but you want the params | **look at the outbound request's** body/headers, not at the stub response |
| a 400 mentions a nested field | mind the outer wrapper `data`/`bizData`/`variables` |
| GraphQL shows only the operation name | expand the `variables` JSON; find `$var: Type` statically |

---
