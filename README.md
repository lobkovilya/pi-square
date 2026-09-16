# pi-square

`pi-square` runs [pi](https://github.com/earendil-works/pi) in a Linux
user/mount/PID namespace. The project directory is writable, while the host
filesystem is replaced by a small set of read-only system mounts: `/etc`,
`/usr`, `/bin`, `/sbin`, `/lib`, `/lib32`, `/lib64`, and on NixOS
`/nix/store`, `/run/current-system`, and `/run/wrappers`, whichever exist.
`pi` and its runtime must be reachable through these mounts; the home
directory is not exposed.

The only persistent writable exception is `~/.pi` (pi auth and sessions).
`~/.gitconfig`, `~/.config/git`, `~/.ssh/config`, `~/.ssh/known_hosts`, and
`~/.config/gh/config.yml` are mounted read-only when present. SSH private keys
and `~/.config/gh/hosts.yml` are never mounted: SSH authenticates through the
forwarded `$SSH_AUTH_SOCK` agent socket, and GitHub authenticates with the
tokens described below. `/tmp` and `$XDG_RUNTIME_DIR` are private temporary
filesystems. Network access and the controlling terminal are retained.

The binary embeds `gh-mode.ts` and loads it only for pi processes launched by
`pi-square`; no separate extension installation is needed. GitHub operations
start in read-only `browse` mode. Use `/gh-mode local` for local Git changes
and `/gh-mode publish` for remote writes, or press Alt+Super+G to cycle modes.

| Mode      | `.git`    | GitHub token | SSH agent | `$HOME`   |
|-----------|-----------|--------------|-----------|-----------|
| `browse`  | read-only | read-only    | hidden    | throwaway |
| `local`   | writable  | read-only    | hidden    | throwaway |
| `publish` | writable  | write (default) | available | real      |

A `gh` command may explicitly select the read-only token in any mode with
`GH_TOKEN=$PI_GH_RO_TOKEN gh ...`; `PI_GH_W_TOKEN` remains usable only in
`publish` mode.

In `browse` and `local` modes every shell command runs in a nested mount
namespace that enforces the table above, so raw HTTP or SSH clients cannot
bypass the mode.

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
pi-square [pi arguments...]
```

On the first run, `pi-square` asks (without echoing input) for a read-only
GitHub token and a write-capable GitHub token. It stores them in
`~/.pi-square/github-tokens.json` with owner-only permissions. Later runs load
the saved tokens without prompting. The credentials file is not mounted into
the sandbox; only the bundled extension and the `pi-square` binary itself are
materialized there.

For safety, the filesystem root and `$HOME` themselves cannot be used as the
project directory.

## Limitations

- pi's own credentials under `~/.pi` are readable by the agent, because pi
  needs them. Anything else the agent must not see has to stay out of `~/.pi`,
  the project directory, and the read-only configuration files listed above.
- In `publish` mode the write token and the SSH agent are available to every
  command the agent runs.
- Network access is not restricted in any mode.
