# Go/Ginkgo end-to-end tests

The opt-in Linux suite drives real interactive pi through a controlling PTY and exercises the production pi-square sandbox and gateway. Ginkgo specs show the explicit `pi-square` arguments, `!`/`!!` prefix, and shell command. The harness uses Gomega, `creack/pty`, and a pure-Go VT emulator; it does not use npm, TypeScript, Cucumber, `script`, or `stty`.

Ordinary tests include deterministic framing, terminal-redraw, quoting, redaction, cancellation, and descendant-cleanup regressions:

```sh
go test ./...
```

Local E2E uses a fake host token and makes no successful upstream request:

```sh
go test -tags=e2e ./e2e -count=1 -timeout=15m -args -ginkgo.label-filter='!live'
```

A run without a label filter also defaults to `!live`. Specs run sequentially and are not retried.

## Live fixture

Live selection runs seven append-only specs: issue creation and an httpbin HTTPS GET in `browse`, `local`, and `publish`, plus a publish-mode Git push of a throwaway branch. Set:

- `PI_SQUARE_E2E_REPOSITORY=OWNER/REPOSITORY`, a dedicated private repository with issues enabled, default branch `main`, and a one-line `README.md`: `Private fixture repository for pi-square sandbox integration tests.`
- `PI_SQUARE_E2E_GITHUB_TOKEN`, a repository-scoped PAT with Metadata read, Contents read/write, and Issues read/write.

```sh
go test -tags=e2e ./e2e -count=1 -timeout=15m -args -ginkgo.label-filter=live
```

Missing prerequisites fail an explicitly requested live run. Created issues are never retried, closed, or deleted. Verification lists issues updated since the spec started through the REST API rather than relying on search indexing; a failing live spec does not skip the others.

Create a fixture only as a separately authenticated, explicitly confirmed operation:

```sh
PI_SQUARE_E2E_REPOSITORY=OWNER/REPOSITORY \
  go run ./e2e/cmd/fixture-init --confirm=OWNER/REPOSITORY
```

The initializer refuses to proceed unless a GitHub 404 proves absence; authentication and network failures are not treated as absence.

## Isolation and diagnostics

By default the suite builds pi-square once in a temporary directory. `PI_SQUARE_E2E_EXECUTABLE` selects an executable. `PI_SQUARE_E2E_CHECKOUT` selects the live checkout; the default is the user cache directory. Existing checkouts require an adjacent ownership marker, the exact repository root, and expected origin. A PID lock prevents concurrent use.

Each session gets isolated pi state under `~/.pi`, disables sessions and discovery, starts offline, and loads only `testdata/no-model.ts`, which exits on accidental model input. PTY sessions use 240×80 geometry and a 10,000-line scrollback. Nonce-delimited base64 frames preserve combined command stdout/stderr, binary bytes, and shell status. Truncation, malformed frames, shell failure, pi exit, signal, and timeout remain distinct. Failures show centrally redacted executable arguments, cwd, command input, and reconstructed terminal output.

Prerequisites are Linux user/mount/PID namespaces, Go, real pi, gh, Git, curl, and base64. Live runs additionally require network access and fixture credentials. pi remains a JavaScript system under test; CI installs pinned pi 0.84.4, but no JavaScript package installation is used by the test harness.