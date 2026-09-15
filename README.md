# pi-square

`pi-square` runs [pi](https://github.com/badlogic/pi-mono) in a Linux
user/mount/PID namespace. The project directory is writable, while the host
filesystem is replaced by a small set of read-only system mounts.

Persistent writable exceptions, matching `pi-safe.sh`, are `~/.pi` (pi auth
and sessions) and `~/.config/gh` (when present). `/tmp` and
`$XDG_RUNTIME_DIR` are private temporary filesystems. Network access and the
controlling terminal are retained.

## Build

```sh
go build -o pi-square .
```

The kernel must permit unprivileged user namespaces. No setuid helper or
runtime sandbox dependency is used.

## Use

Run it from a project directory:

```sh
pi-square [pi arguments...]
```

For safety, the filesystem root and `$HOME` themselves cannot be used as the
project directory.
