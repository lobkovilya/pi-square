# Live BDD smoke tests

This opt-in Cucumber.js suite runs exactly six black-box scenarios against real GitHub and `https://httpbin.org`: issue creation and HTTPS GET in `browse`, `local`, and `publish` modes. It has no mocks and makes no model calls.

## Fixture and token

Set `PI_SQUARE_E2E_REPOSITORY` to a dedicated private fixture repository in `OWNER/REPOSITORY` form. The repository must have issues enabled and a one-line `README.md` on `main`:

```
Private fixture repository for pi-square sandbox integration tests.
```

After the repository exists, issue a fine-grained PAT selected only for that repository. It needs **Metadata: read**, **Contents: read** for checkout, and **Issues: read and write**. Export it only as `PI_SQUARE_E2E_GITHUB_TOKEN`; never put it in a URL, config file, command argument, or committed file.

Repository creation uses the account already authenticated by `gh auth login` because a repository-scoped fine-grained PAT cannot be issued before its repository exists. Creation is separately guarded because the spelling must be explicitly confirmed:

```sh
cd e2e
PI_SQUARE_E2E_REPOSITORY=OWNER/REPOSITORY npm run fixture:init -- --confirm=OWNER/REPOSITORY
```

The harness removes `PI_SQUARE_E2E_GITHUB_TOKEN` from children and maps its value to `GH_TOKEN` only for host-side `gh`, clone operations, and `pi-square` startup. `pi-square` keeps the real token in the host gateway and removes it before starting pi or sandbox commands; commands receive only the production dummy credential.

## Install and run

Prerequisites are Linux with unprivileged user namespaces, Node.js/npm, Go (unless selecting a prebuilt binary), pi, `gh`, Git, curl, util-linux `script`, `stty`, `base64`, and access to GitHub and httpbin. The suite fails setup rather than skipping when any prerequisite is unavailable.

```sh
cd e2e
npm ci
PI_SQUARE_E2E_REPOSITORY=OWNER/REPOSITORY PI_SQUARE_E2E_GITHUB_TOKEN=... npm run live
```

By default the suite builds `../pi-square` once into `e2e/.tmp/`. Select an existing executable with `PI_SQUARE_E2E_EXECUTABLE=/absolute/path/to/pi-square`. Select a dedicated checkout with `PI_SQUARE_E2E_CHECKOUT=/absolute/test-owned/path`; the default is `$XDG_CACHE_HOME/pi-square/e2e/checkout` (or `~/.cache/...`). Existing directories are reset only when an adjacent ownership marker identifies the expected repository and `origin` is correct. An adjacent lock prevents concurrent reset/use.

Each scenario starts real interactive pi in a controlling PTY:

```text
pi-square --gh-mode=<mode> -- --no-session ...
> ! command
```

The harness waits for the mode footer and a `/gh-mode status` round trip, pastes `!` input, captures the result, and exits with `/quit`. The production `user_bash` handler routes both `!` and `!!` through the same launcher, supervisor, filesystem/network namespaces, and gateway as the model's bash tool. No test-only execution command exists. Setup probes exercise both prefixes and verify a nonzero shell exit code.

A shell wrapper frames the command's combined stdout/stderr and exit code as base64 between per-command nonce markers, each on its own line so terminal wrapping cannot split them. A headless terminal reconstructs wrapped lines and ANSI redraws; echoed editor text cannot match the markers because printf assembles them from split arguments. pi shows only the last screenful of a completed command until expanded, so the driver presses the tool-output toggle until the whole frame is visible. `stdout` in results is the decoded combined command output; `stderr` contains harness diagnostics and a terminal snapshot on failure. Output exceeding pi's truncation limits fails capture immediately rather than silently passing.

Each invocation uses a fresh ephemeral session and isolated agent directory, disables extension/skill/template/context discovery, and starts offline. The only test extension is a fail-fast model-input tripwire: it never executes commands or changes shell routing, and terminates if an agent turn is attempted. Temporary agent directories are removed after each invocation.

Sessions have a 75-second deadline (including startup and clean exit); failed sessions are killed as a process group, with production parent-death cleanup for sandbox descendants. Curl also has connect and total deadlines. Failures report mode, shell status, redacted output, and elapsed time. Runs are sequential and Cucumber should report exactly **6 scenarios**.

Run local PTY regression tests without GitHub credentials or network requests:

```sh
npm test
```

These still require pi and production sandbox prerequisites, and take about fifteen seconds. They build the binary once, hand `pi-square` a fake host token that must never reach a command, and drive multi-step interactive sessions through `src/terminal.ts` (`PiSession`). They verify:

- `!` and `!!` route through the production sandbox; output and exit-code capture is lossless for wrapped, binary, empty, and signal-terminated output; a hung command fails the deadline without a false result.
- `browse` is the default without `--gh-mode`; `/gh-mode` switches apply to newly launched commands, including `toggle`; a command that is already running keeps the permissions it launched with.
- In every mode, commands see only the dummy `GH_TOKEN`, the gateway proxy, no `PI_SQUARE_*` variables, their own PID namespace, a masked control socket and gateway directory, a hidden host `/tmp`, and a throwaway home outside `publish`.
- In `browse` and `local`, the gateway denies REST writes and GraphQL mutations with `write_requires_publish` and a GitHub-style top-level `message` that `gh` displays, without opening the escalation dialog or changing the mode.
- Commands cannot bypass the proxy, and the gateway refuses plain HTTP, private and loopback destinations, and ports other than 443.

Everything in `npm test` is decided inside the gateway, so no request leaves the machine.

## Append-only policy

Every scenario uses a UUID. Created issues remain open permanently, including any unexpected issue from a failed denial test. Nothing remotely created by the suite is closed, deleted, or reused. The harness paginates repository issue listings for exact identifier verification rather than relying on GitHub search indexing.
