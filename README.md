# pi-square

`pi-square` runs [pi](https://github.com/earendil-works/pi) in a Linux
user/mount/PID namespace and routes every shell command it launches through a
mandatory GitHub API gateway. The project directory is writable, while the host
filesystem is replaced by a small set of read-only system mounts: `/etc`,
`/usr`, `/bin`, `/sbin`, `/lib`, `/lib32`, `/lib64`, and on NixOS
`/nix/store`, `/run/current-system`, and `/run/wrappers`, whichever exist.
`pi` and its runtime must be reachable through these mounts; the home
directory is not exposed.

The only persistent writable exception is `~/.pi` (pi auth and sessions).
`~/.gitconfig`, `~/.config/git`, `~/.ssh/config`, `~/.ssh/known_hosts`, and
`~/.config/gh/config.yml` are mounted read-only when present. GitHub
credentials never enter the sandbox: the gateway holds the host `gh`
credential and adds it to approved requests itself. `/tmp` and
`$XDG_RUNTIME_DIR` are private temporary filesystems.

The binary embeds `gh-mode.ts` and loads it only for pi processes launched by
`pi-square`; no separate extension installation is needed.

## Permission model

GitHub operations start in read-only `browse` mode. Use `/gh-mode local` for
local Git changes and `/gh-mode publish` for remote writes, or press
Alt+Super+G to cycle modes.

| Mode      | Workdir   | Shell network                                                       |
|-----------|-----------|---------------------------------------------------------------------|
| `browse`  | read-only | HTTPS; gateway-authenticated GitHub REST GET/HEAD and GraphQL queries |
| `local`   | writable  | HTTPS; gateway-authenticated GitHub REST GET/HEAD and GraphQL queries |
| `publish` | writable  | HTTPS; adds gateway-authenticated REST writes and GraphQL mutations   |

The footer summarizes the mode for **new** commands, in Workdir · Local Git ·
GitHub API · Net order:

```text
browse ·  ro ·  ro ·  ro ·  rw
local ·  rw ·  rw ·  ro ·  rw
publish ·  rw ·  rw ·  rw ·  rw
```

| Mode | Workdir | Local Git | GitHub API | Net |
|------|---------|-----------|------------|-----|
| `browse` | RO | RO | RO | RW |
| `local` | RW | RW | RO | RW |
| `publish` | RW | RW | RW | RW |

The icons are Nerd Font folder (``), Git branch (``), GitHub (``),
and globe (``); they require a Nerd Font terminal. `/gh-mode` or
`/gh-mode status` gives named, font-independent explanations. A plain-text
rendering of the browse row is `workdir:ro · git:ro · gh-api:ro · net:rw`.
OFF means no access, RO means read-only within the stated scope, and RW means
reads and writes within that scope, **not** unrestricted access. No current
mode has an OFF resource or a separate resource toggle.

Workdir means the exposed project directory, not the entire host filesystem;
system mounts, private temporary directories, and pi state exceptions are
unchanged. Local Git follows Workdir access for repository changes there:
there is no independent Git gate. Remote Git operations also depend on the
network and destination policy; external Git directories and worktrees are
not necessarily protected by a dedicated metadata mechanism.

The GitHub icon means **gateway-authenticated `api.github.com` REST/GraphQL**,
not GitHub-wide read-only access. RO allows approved REST GET/HEAD and GraphQL
queries; RW also allows approved REST writes and GraphQL mutations. Other
GitHub hosts use raw HTTPS tunnels, and HTTPS Git pushes are not governed by
this API indicator. Independently available credentials are outside the
gateway-credential guarantee.

Net means other public HTTPS traffic through the mandatory gateway, including
non-API GitHub hosts. RW allows requests beyond reads, not arbitrary network
access. The indicators overlap (Git uses files; GitHub uses networking); they
summarize mode effects, not independently selectable grants. They describe
sandboxed commands, not pi's provider connection or in-process extensions.

GitHub authentication is not the read boundary. The gateway enforces which
operations may use its credential before forwarding them upstream, so a
read-only mode cannot use that credential for writes regardless of its scopes.
This guarantee does not cover credentials independently available to a command.

HTTPS on port 443 is reachable only through the gateway. For
`api.github.com:443`, the gateway intercepts TLS and applies the REST/GraphQL
policy above. Connections to other public destinations—including other GitHub
hosts and external relays—are end-to-end TLS tunnels: the gateway neither
inspects traffic nor injects its credential.
Private, loopback, link-local, and other non-public destinations are rejected.
Plain HTTP, SSH, and other destination ports are unsupported. Direct
connections, alternative proxies, and proxy bypass do not work; a command that
ignores the proxy simply fails.

### Downgrade behavior

Permissions are assigned when a command launches and stay fixed for its
lifetime. Switching away from `publish` affects future commands only:
write-enabled commands that are already running, including background
descendants, keep write access until they exit. Switching into `publish` does
not grant write access to commands that already started in a read-only mode; a
denied request stays denied, so the agent must launch a new command after the
mode changes.

When a model bash command is denied because it performs a write, `gh-mode` offers to
switch to `publish` or keep the current mode. Accepting the switch does not
replay the command; the agent launches it again. Interactive `! command` and
`!! command` use the same sandbox and launch-time permissions. After a denial,
switch explicitly with `/gh-mode publish` and rerun the interactive command.

## Trust boundary

Pi and the bundled extension are trusted and keep normal host networking so pi
can reach its model provider. Every shell command, in every mode, runs through
the isolated command runner and can reach nothing but the gateway.

A shared host-user gateway owns the credential and CA; each trusted supervisor
owns its network namespaces and command execution. Pi asks it to launch commands over a private control channel that
launched commands cannot reach. Commands run capability-less in their own
network, mount, and PID namespaces; they never receive real GitHub
credentials, the gateway's TLS keys, the control channel, or a handle to the
write-enabled network namespace. This targets accidental agent writes and
adds defense in depth against command code; it is not a sandbox for hostile
code running inside a trusted pi extension.

## Authentication

When a gateway instance starts, it snapshots the host GitHub credential with
`gh auth token --hostname github.com`, so `gh` must already be authenticated
for GitHub.com (`gh auth login`). Attaching another session does **not** refresh
it: restart the instance after changing `gh` authentication. The credential is
held only in the gateway's memory; it is never written to disk, logged, or
passed into pi or any command. Each instance also holds its private CA key in
memory; supervisors receive only its public certificate, and restarting rotates
that CA. The `api.github.com` leaf certificate is short-lived and renewed by
the instance itself, so a long-running instance never serves an expired one.

For every approved request the gateway strips any client-supplied
authorization, cookies, and proxy credentials, inserts its own credential, and
sends the request to the TLS-verified `api.github.com` upstream. Commands
receive a dummy `GH_TOKEN` so `gh` and `curl` construct authenticated calls;
the gateway replaces it.

## Build

```sh
go build -o pi-square .
```

The kernel must permit unprivileged user namespaces. No setuid helper or
runtime sandbox dependency is used.

A Nix flake is also provided for x86-64 and AArch64 Linux:

```sh
nix build
nix run .
```

## Tests

```sh
go test ./...
go test -tags=e2e ./e2e -count=1 -timeout=15m -args -ginkgo.label-filter='!live'
```

The tagged Ginkgo suite drives real interactive pi through a Go PTY and the production sandbox. It defaults to local-only tests and needs no npm test harness. The separately authorized six-case live suite, fixture setup, credential policy, and diagnostics are documented in [`e2e/README.md`](e2e/README.md).

`TestIntegration` drives read-only commands through the real sandbox and gateway to GitHub.com using the host `gh` credential. It is opt-in, uses a temporary gateway instance, and stops it afterwards:

```sh
PI_SQUARE_INTEGRATION=1 go test -run TestIntegration -v
```

## Use

Run it from a project directory:

```sh
pi-square [options] [-- pi arguments...]

pi-square --version       # pi-square version
pi-square --help          # pi-square help
pi-square --mode=dev      # sandbox normally, without GitHub mode prompt guidance
pi-square --gh-mode=local # explicitly select the initial GitHub mode
pi-square --gateway=team  # attach to an existing named instance
pi-square gateway list    # show actual health and live session count
pi-square gateway start team
pi-square gateway stop team
pi-square gateway stop team --force
pi-square -- --version    # pi version
pi-square -- --model example "Explain this project"
```

By default, `pi-square` atomically starts or attaches to the shared `default`
gateway for the host user. An explicit `--gateway=NAME` (even `default`) requires
an already healthy instance; create one with `pi-square gateway start NAME`.
Instances outlive pi-square sessions. `gateway stop` refuses while sessions are
attached; `--force` stops it even with active sessions and long-running commands
lose gateway access (there is no direct-network fallback). State and sockets
live under `$XDG_RUNTIME_DIR/pi-square/instances/` when set, otherwise under
`$XDG_CACHE_HOME/pi-square/instances/` (default `~/.cache`); no credentials or
CA private keys are stored there. Each instance writes its request and tunnel
log to `gateway.log` in its state directory, truncated when the instance
starts; log lines carry methods, paths, and destinations, never credentials or
bodies. Stale sockets do not count as healthy instances. The wrapper and
gateway must speak the same protocol version; restart an old instance after
upgrading.

All pi arguments (including prompts) must follow `--`. Running `pi-square`
without arguments starts pi normally. `--mode=dev` keeps the sandbox and
GitHub-mode enforcement active but omits mode information from the model's
system prompt. Versions use `0.0.0-preview.v<shortCommitHash>` for both Nix and
local `go build` builds. Override the version with
`go build -ldflags "-X main.version=VERSION" -o pi-square .`.

For safety, the filesystem root and `$HOME` themselves cannot be used as the
project directory.

## Limitations

- Network isolation applies to commands, not to pi itself or to in-process
  custom tools. Audit or disable any custom tool that performs its own network
  I/O; do not assume all pi traffic is isolated.
- Only `api.github.com` REST and GraphQL receive the gateway credential and
  policy enforcement. Other public HTTPS destinations are raw tunnels. Git
  operations that need SSH or non-HTTPS ports remain unsupported; HTTPS Git,
  LFS, registries, assets, raw-content hosts, and GitHub Enterprise may be
  reachable but receive no gateway-injected authentication.
- pi's own credentials under `~/.pi` are readable by the agent, because pi
  needs them. Anything else the agent must not see has to stay out of `~/.pi`,
  the project directory, and the read-only configuration files listed above.
- If the gateway fails, API access fails closed. There is no direct-network
  fallback.
