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

| Mode      | Workdir   | Shell network                                    |
|-----------|-----------|--------------------------------------------------|
| `browse`  | read-only | `api.github.com` REST GET/HEAD and GraphQL queries |
| `local`   | writable  | `api.github.com` REST GET/HEAD and GraphQL queries |
| `publish` | writable  | adds REST writes and GraphQL mutations           |

GitHub authentication is not the read boundary. The gateway enforces which
operations are permitted before forwarding them upstream, so a read-only mode
stays read-only regardless of the credential's scopes.

Only `api.github.com:443` is reachable, and only through the gateway. Git
transport, SSH, Git LFS, registries, release asset hosts, raw-content hosts,
and every other network destination are unsupported from commands. Direct
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

When a command is denied because it performs a write, `gh-mode` offers to
switch to `publish` or keep the current mode. Accepting the switch does not
replay the command; the agent launches it again.

## Trust boundary

Pi and the bundled extension are trusted and keep normal host networking so pi
can reach its model provider. Every shell command, in every mode, runs through
the isolated command runner and can reach nothing but the gateway.

A trusted supervisor owns the network namespaces, the gateway, and command
execution. Pi asks it to launch commands over a private control channel that
launched commands cannot reach. Commands run capability-less in their own
network, mount, and PID namespaces; they never receive real GitHub
credentials, the gateway's TLS keys, the control channel, or a handle to the
write-enabled network namespace. This targets accidental agent writes and
adds defense in depth against command code; it is not a sandbox for hostile
code running inside a trusted pi extension.

## Authentication

`pi-square` reads the host GitHub credential at startup with
`gh auth token --hostname github.com`, so `gh` must already be authenticated
for GitHub.com (`gh auth login`). The credential is held only in the gateway's
memory; it is never written to disk, logged, or passed into pi or any command.

For every approved request the gateway strips any client-supplied
authorization, cookies, and proxy credentials, inserts its own credential, and
sends the request to the TLS-verified `api.github.com` upstream. Commands
receive a dummy `GH_TOKEN` so `gh` and `curl` construct authenticated calls;
the gateway replaces it.

The previous read-only/write token file at `~/.pi-square/github-tokens.json`
is no longer used. `pi-square` does not read or delete it; you can remove it
manually.

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

## Use

Run it from a project directory:

```sh
pi-square [options] [-- pi arguments...]

pi-square --version       # pi-square version
pi-square --help          # pi-square help
pi-square --mode=dev      # sandbox normally, without GitHub mode prompt guidance
pi-square -- --version    # pi version
pi-square -- --model example "Explain this project"
```

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
- Only `api.github.com` REST and GraphQL are supported. `git clone/fetch/push`,
  SSH, Git LFS, registries, asset uploads and downloads, raw-content hosts, and
  GitHub Enterprise are out of scope.
- pi's own credentials under `~/.pi` are readable by the agent, because pi
  needs them. Anything else the agent must not see has to stay out of `~/.pi`,
  the project directory, and the read-only configuration files listed above.
- If the gateway fails, API access fails closed. There is no direct-network
  fallback.
