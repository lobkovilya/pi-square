# pi-square

`pi-square` runs [pi](https://github.com/badlogic/pi-mono) in a Linux
user/mount/PID namespace. The project directory is writable, while the host
filesystem is replaced by a small set of read-only system mounts.

Persistent writable exceptions, matching `pi-safe.sh`, are `~/.pi` (pi auth
and sessions) and `~/.config/gh` (when present). `/tmp` and
`$XDG_RUNTIME_DIR` are private temporary filesystems. Network access and the
controlling terminal are retained.

The binary embeds `gh-mode.ts` and loads it only for pi processes launched by
`pi-square`; no separate extension installation is needed. GitHub operations
start in read-only `browse` mode. Use `/gh-mode local` for local Git changes
and `/gh-mode publish` for remote writes, or press Alt+Super+G to cycle modes.

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
the sandbox; only the bundled extension is materialized there.

For safety, the filesystem root and `$HOME` themselves cannot be used as the
project directory.
