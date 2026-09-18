# GitHub API gateway implementation plan

## Goal

Replace the read-only/write token approach with a mandatory GitHub API gateway. GitHub authentication permissions are not the read-mode security boundary: the gateway enforces permitted operations before sending them upstream.

Initial scope: GitHub.com REST and GraphQL APIs on `api.github.com:443` only. Git transport, SSH, LFS, registries, release asset hosts, raw-content hosts, and all other network destinations are unsupported from isolated commands.

No implementation is included in this plan.

## Permission model

| Mode at command launch | Workdir | Network environment | API policy |
| --- | --- | --- | --- |
| `browse` | Read-only | RO namespace | REST reads and GraphQL queries |
| `local` | Writable | RO namespace | REST reads and GraphQL queries |
| `publish` | Writable | W namespace | REST reads/writes and GraphQL queries/mutations |

Permissions are assigned when a command launches and remain fixed for its lifetime.

**Downgrade behavior: launch-time permissions.** Switching away from publish affects future commands only. Existing write-enabled commands, including background descendants, retain write access until they exit. Do not kill them, close their connections, or change their gateway policy on downgrade. Conversely, existing RO commands never gain write access when the user enables publish.

The UI must clearly explain these semantics and, where practical, show surviving write-enabled jobs. A request denied in an RO command remains denied after a mode change; the agent must launch a new command after approval.

## Trust boundary

Pi remains on the normal host network so it can contact its model provider. Every shell command, in every mode, executes through the isolated command runner.

Pi, the bundled extension, and any other in-process extensions/tools are trusted. Network operations performed directly inside the pi process are outside this enforcement boundary. Audit, disable, or adapt custom tools that would bypass the isolated runner. Do not claim that all pi traffic is isolated.

Commands and their descendants are untrusted. They must not receive:

- Real GitHub credentials.
- TLS private keys.
- Trusted launcher control descriptors.
- Namespace handles or connections granting access to the other network environment.
- An agent-callable interface that can select write permissions independently of the trusted launcher.

Keep existing filesystem sandbox protections. This project targets accidental agent writes, not hostile code executing inside trusted pi extensions.

## Architecture

```text
Trusted pi extension / command launcher       Normal network: model API access
    |
    +-- browse/local command --> RO netns --> localhost RO frontend --+
    |                                                               |
    +-- publish command ------> W netns  --> localhost W frontend ----+
                                                                    |
                                                 External gateway process
                                                 +-- RO ingress: fixed reads
                                                 +-- W ingress: fixed writes
                                                                    |
                                                        api.github.com:443
```

Use one external gateway process with two immutable-policy ingress endpoints. Both share GitHub authentication, TLS handling, HTTP transport, and logging. There is no gateway mode state and no `set-mode` protocol.

Each network namespace has only enabled loopback and its localhost proxy listener. It has no external interface, default route, or direct DNS connectivity. Use separate mount/PID isolation as needed for command filesystem permissions; browse and local share the RO network environment without sharing workdir writability.

The frontend is a byte relay between its localhost TCP listener and its assigned gateway ingress. It has no credentials and cannot select policies or arbitrary upstream destinations.

### Enforcing ingress identity

The gateway trusts which fixed ingress accepted the connection, never a client header, URL parameter, or frontend-supplied policy label. It cannot infer trustworthy namespace identity merely from bytes sent over a Unix socket.

Use OS-enforced access separation:

- RO frontend can reach only RO ingress.
- W frontend can reach only W ingress.
- RO commands cannot reach W ingress or its frontend through filesystem sockets, inherited descriptors, namespace handles, or mounted runtime paths.
- Prefer private inherited channels. If pathname Unix sockets are used, expose only the appropriate socket in each mount environment and hide parent runtime directories.
- Accessing an exposed data socket directly must still invoke its fixed gateway policy.

Two network namespaces alone do not isolate filesystem Unix sockets. Descriptor inheritance and mounts are part of the security boundary.

### Trusted command dispatch

A supervisor owns the namespace workers and command execution lifecycle. Pi requests command launches through a private control channel inaccessible to tool subprocesses. The trusted extension selects RO or W based on the mode captured at launch.

Do not implement an unprotected `pi-square run --write` helper that commands can invoke to escalate. Internal stage arguments are not authorization. Only trusted supervisor capabilities may authorize W launches.

The supervisor does not need session mode state: each trusted launch request specifies the fixed execution class. Acknowledge launches and track jobs without exposing the control channel to launched commands.

## Authentication

Assume host-side `gh` is already authenticated for GitHub.com.

At startup:

1. Run host-side `gh auth token --hostname github.com` and capture its output privately.
2. Fail with an actionable error if authentication is unavailable.
3. Keep the credential in external gateway memory; never log it or pass it into pi or commands.
4. Give isolated clients a dummy `GH_TOKEN` so `gh` can construct authenticated API calls.

For every approved upstream request:

- Strip supplied Authorization, cookies, and proxy authentication headers.
- Insert the gateway-owned GitHub credential using the appropriate upstream authorization scheme.
- Send it only to the fixed, TLS-verified `api.github.com` upstream.

Remove inherited GitHub token variables before launching pi and commands, then install only the dummy command credential. Continue excluding GitHub credential files from sandbox mounts. Audit Git config, credential helpers, and inherited environment for unintended credential exposure. The gateway cannot grant privileges absent from the host credential.

Remove the custom RO/W token provisioning requirement. Do not silently delete existing user token files during migration; document that they are unused and can be removed manually.

## Proxy and TLS behavior

Configure command environments with a mandatory explicit proxy:

```text
HTTPS_PROXY=http://127.0.0.1:<port>
HTTP_PROXY=http://127.0.0.1:<port>
NO_PROXY=
```

Normalize corresponding lowercase variables and remove conflicting inherited proxy settings. These variables enable compatibility; absence of external connectivity enforces the boundary. A client ignoring the proxy must fail, not receive fallback connectivity.

Gateway flow:

1. Accept only HTTP CONNECT to `api.github.com:443`.
2. Terminate client TLS using an ephemeral CA and a certificate for `api.github.com`.
3. Parse, validate, and authorize each HTTP request.
4. Forward through a standard HTTP transport to the fixed upstream with normal certificate verification.
5. Return the upstream response without following redirects inside the gateway.

Private CA/leaf keys remain external. Mount public trust material read-only and configure supported clients, initially `gh` and curl. Use a CA bundle containing normal roots plus the gateway CA. Add other runtime-specific trust settings only as needed.

Start with client-facing HTTP/1.1. Use maintained HTTP and GraphQL libraries rather than custom parsers.

Common validation:

- Require the CONNECT authority, TLS server name, and HTTP authority to match the permitted hostname and port.
- Reject alternative authorities, nested CONNECT, protocol upgrades, and unsupported request encodings.
- Use standard parser protections for conflicting framing and reject ambiguous requests.
- Construct upstream requests from validated parsed fields; do not blindly relay decrypted request bytes.
- Strip hop-by-hop headers, proxy credentials, and upstream response cookies where appropriate.
- Apply header/body limits, idle timeouts, and concurrency limits.
- Classify normalized routes consistently; reject ambiguous GraphQL path variants rather than let them fall through REST rules.
- Do not follow redirects. Client-followed redirects to other hosts are blocked.
- Disable inherited outbound proxy settings in the gateway transport unless explicitly supported later.

## Fixed RO policy

### REST

Allow `GET` and `HEAD` on supported REST routes. Deny other methods.

This deliberately excludes read-like POST operations. Add exceptions only after explicit review and tests.

### GraphQL

Handle the GraphQL endpoint before the REST method rule. Initially accept only:

```text
POST /graphql
Content-Type: application/json
```

Require a single JSON object with a `query` string and optional `operationName` and variables. Parse the document into a GraphQL AST and resolve the selected operation using GraphQL operation-selection rules.

Allow only a selected `query` operation. Reject:

- Selected mutations or subscriptions.
- Missing, unknown, or ambiguous operation selection.
- Malformed JSON or GraphQL, including ambiguous duplicate JSON fields.
- Request batches.
- Persisted queries and unsupported extension mechanisms.
- Unsupported body compression/encodings.
- GraphQL-over-GET and other unsupported endpoint forms.

Fragments and aliases must not affect operation classification. For multiple operations, require a valid `operationName`. Bound document/body size and parser resource usage.

## Fixed W policy

Permit supported REST methods: GET, HEAD, POST, PUT, PATCH, and DELETE. Permit parsed GraphQL queries and mutations; subscriptions remain unsupported.

Preserve host restrictions, credential replacement, TLS verification, request validation, limits, and denial of tunnels/upgrades. W ingress is API write permission, not general internet access.

## Extension changes

Update `gh-mode.ts` to:

- Retain mode UI, shortcuts, user confirmation, and browse-mode edit/write guards.
- Route every shell invocation through the trusted isolated launcher, including publish invocations and commands without literal `gh` or `git` tokens.
- Capture the execution class once per launch.
- Remove RO/W token selection, token-prefix generation, Git askpass generation, and command-string classification for API authorization.
- Remove escalation inferred from generic GitHub command failures.
- Start new sessions in browse; do not silently restore publish from persisted state.
- Explain that mode changes affect future commands only and that existing publish jobs remain write-enabled.

Gateway denials should use recognizable error codes, such as:

- `write_requires_publish`
- `unsupported_host`
- `unsupported_request_format`

Where practical, deliver authenticated denial events through trusted infrastructure and associate them with the originating job. Shared namespaces/listeners do not automatically provide reliable command identity; never trust a command-supplied identifier as authority. If correlation is unavailable, report the denial to the command and avoid guessing which command caused a gateway event.

Offer escalation only for a genuine write-policy denial. Do not automatically replay HTTP requests or whole shell commands, since a command may already have performed other side effects.

Append mode guidance to the system prompt:

> Shell networking is limited to api.github.com through a mandatory proxy. Browse/local permit REST GET/HEAD and GraphQL queries. New commands launched in publish may perform API writes. Git transport, SSH, other GitHub hosts, and other internet access are unsupported. Direct networking and proxy bypass do not work. Mode changes affect newly launched commands only.

Prompt guidance assists cooperation; it is not enforcement.

## Wrapper and lifecycle changes

Refactor `main.go` responsibilities into testable components as appropriate:

1. Host authentication resolution and credential sanitization.
2. External fixed-policy gateway and ephemeral TLS material.
3. Trusted supervisor and private command-launch protocol.
4. RO/W network namespace workers and localhost frontends.
5. Per-command filesystem/PID sandbox setup.
6. Job tracking, stdout/stderr forwarding, cancellation, and cleanup.

Bring loopback up during namespace setup with only the necessary setup capabilities, then drop capabilities before untrusted code runs. Verify namespace ownership prevents commands from joining the other network namespace. Nested user namespaces must not confer access to W capabilities.

Retain private PID/proc mounts where needed to prevent access to trusted parent descriptors and mounts. Audit all inherited descriptors, mounted sockets, and runtime directories. Remove SSH-agent forwarding for this API-only scope.

Preserve shell tool behavior: working directory, environment sanitization, output streaming, exit status, timeouts, cancellation, and stdin handling.

Track descendants so background processes retain their launch permissions but cannot escape final session cleanup. On mode change, leave existing jobs and connections untouched. On session exit, terminate remaining jobs and frontends, stop the gateway, and remove temporary sockets/certificates. If the gateway fails, API access fails closed; no direct networking fallback is permitted.

Keep the implementation rootless. Prove the namespace worker/supervisor design on supported kernels before integrating it into the extension; document required unprivileged-user-namespace support.

## Implementation sequence

### Phase 1: prove isolation and dispatch

- Build a minimal trusted supervisor with RO/W workers and distinct fixed ingress paths.
- Verify loopback-only networking and rootless setup.
- Verify RO commands cannot reach W sockets, descriptors, namespace handles, or launch controls.
- Verify pi retains model-provider connectivity.
- Exercise cancellation and background descendants.

### Phase 2: implement gateway

- Resolve host `gh` authentication.
- Implement exact-host CONNECT and TLS interception.
- Implement shared validation and fixed RO/W policies.
- Add structured denials and secret-safe metadata logging.
- Test with `gh api` and curl.

### Phase 3: integrate modes and runner

- Route all shell commands through trusted dispatch.
- Map browse/local to RO and publish to W.
- Preserve filesystem guards and launch-time permission semantics.
- Remove token switching and command-string authorization heuristics.
- Add prompt/UI guidance and reliable error handling.

### Phase 4: harden and document

- Audit mounts, descriptors, environment, credentials, and process cleanup.
- Run namespace integration tests and parser/security regression tests.
- Update README with supported API surface, trust boundary, authentication setup, and downgrade semantics.
- Remove obsolete token-related tests/code without overwriting unrelated existing work.

## Acceptance tests

### API policy

- REST GET/HEAD succeeds through both ingress policies.
- REST POST/PUT/PATCH/DELETE is denied through RO and allowed through W, subject to upstream permissions.
- GraphQL query succeeds; mutation fails through RO and succeeds through W.
- Named queries/mutations among multiple operations are correctly selected.
- Aliases, fragments, comments, and operation names do not confuse classification.
- Malformed documents, batches, duplicate JSON fields, ambiguous operation selection, unsupported encodings, and GraphQL GET requests fail closed.
- GraphQL endpoint variants cannot fall through the REST read rule.

### Network and credential boundary

- Direct IPv4/IPv6 connections and direct DNS access fail.
- Clearing or changing proxy variables cannot bypass isolation.
- Non-GitHub hosts, alternative ports, authority mismatches, and protocol upgrades fail.
- Redirects cannot leak authorization to another host.
- RO commands cannot reach W frontend/ingress or invoke a trusted W launch.
- Tool processes cannot inspect real credentials, private TLS keys, supervisor descriptors, or other namespace handles.
- Gateway and frontend failures fail closed.
- Logs and errors omit tokens, cookies, and request bodies by default.

### Lifecycle and UX

- Browse/local/publish commands receive the expected filesystem and network permissions.
- Existing RO jobs remain RO after entering publish.
- Existing W jobs retain writes after downgrade, including background descendants.
- New commands after downgrade use RO ingress.
- Denials do not trigger automatic request or command replay.
- Cancellation and tool timeouts preserve expected behavior.
- Session exit cleans up workers, descendants, sockets, and ephemeral TLS files.
- Pi continues to reach its model provider.

Use a mock upstream for most policy tests. Any live write test must be explicitly opted into and confined to a disposable test repository.

## Deferred scope

- Network isolation of pi itself and in-process custom tools.
- Git clone/fetch/push, SSH, Git LFS, registries, asset uploads/downloads, and additional hosts.
- REST read-like POST exceptions.
- GitHub Enterprise.
- Automatic credential refresh and GitHub App credentials.
- Client-facing HTTP/2.
- Per-request approval or revocation of already-running write-enabled commands.
