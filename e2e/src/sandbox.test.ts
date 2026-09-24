import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { DUMMY_COMMAND_TOKEN, FAKE_HOST_TOKEN, LocalFixture, MODES } from "./local-fixture.js";
import { quote } from "./terminal.js";
import type { Mode } from "./types.js";

let fixture: LocalFixture;
before(async () => { fixture = await LocalFixture.create(); }, { timeout: 180_000 });
after(async () => { await fixture?.dispose(); });

const READ_ONLY_MODES: readonly Mode[] = ["browse", "local"];
const TOUCH = "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi";

function parseLines(output: string): Map<string, string> {
  return new Map(output.split("\n").filter((line) => line.includes("=")).map((line) => {
    const index = line.indexOf("=");
    return [line.slice(0, index), line.slice(index + 1)];
  }));
}

test("browse is the default without --gh-mode", { timeout: 60_000 }, async () => {
  await fixture.withSession(undefined, async (session) => {
    assert.equal((await session.bash(TOUCH)).output, "readonly\n");
  });
});

test("mode switches apply to newly launched ! and !! commands", { timeout: 120_000 }, async () => {
  await fixture.withSession("browse", async (session) => {
    assert.equal((await session.bash(TOUCH)).output, "readonly\n");
    await session.slash("/gh-mode local", "GitHub mode: local");
    assert.equal((await session.bash(TOUCH, "!!")).output, "writable\n");
    await session.slash("/gh-mode browse", "GitHub mode: browse");
    assert.equal((await session.bash(TOUCH)).output, "readonly\n");
    await session.slash("/gh-mode publish", "GitHub mode: publish");
    assert.equal((await session.bash(TOUCH, "!!")).output, "writable\n");
    await session.slash("/gh-mode toggle", "GitHub mode: browse");
    assert.equal((await session.bash(TOUCH)).output, "readonly\n");
  });
});

test("a running command keeps the permissions it launched with", { timeout: 60_000 }, async () => {
  await fixture.withSession("browse", async (session) => {
    const running = session.startBash(`sleep 4; ${TOUCH}`);
    await session.slash("/gh-mode local", "GitHub mode: local");
    assert.equal(running.completed(), false, "the mode switch must land while the browse command is still running");
    assert.equal((await running.result).output, "readonly\n");
    assert.equal((await session.bash(TOUCH)).output, "writable\n");
  });
});

for (const mode of MODES) {
  test(`${mode} commands see only the dummy credential and the restricted root`, { timeout: 60_000 }, async () => {
    await fixture.withSession(mode, async (session) => {
      const probe = [
        `echo GH_TOKEN=$GH_TOKEN`,
        `echo HTTPS_PROXY=$HTTPS_PROXY`,
        `echo HOME=$HOME`,
        `echo PI_SQUARE_VARS=$(env | grep -c '^PI_SQUARE_')`,
        `echo LEAKED_HOST_TOKEN=$(env | grep -c ${quote(FAKE_HOST_TOKEN)})`,
        `echo PID1=$(cat /proc/1/comm)`,
        `echo CONTROL_SOCKET=$(test -S /run/pi-square/control.sock && echo reachable || echo hidden)`,
        `echo GATEWAY_DIR=$(ls /run/pi-square/gateway >/dev/null 2>&1 && echo readable || echo masked)`,
        `echo HOST_TMP=$(test -e ${quote(fixture.hostSecret)} && echo visible || echo hidden)`,
        `echo WORKDIR=$(test -d ${quote(fixture.workdir)} && echo visible || echo hidden)`,
      ].join("; ");
      const result = await session.bash(probe);
      assert.equal(result.status, 0, result.output);
      const values = parseLines(result.output);
      assert.equal(values.get("GH_TOKEN"), DUMMY_COMMAND_TOKEN);
      assert.equal(values.get("HTTPS_PROXY"), "http://127.0.0.1:8877");
      assert.equal(values.get("PI_SQUARE_VARS"), "0");
      assert.equal(values.get("LEAKED_HOST_TOKEN"), "0");
      assert.equal(values.get("PID1"), "bash", "commands must run as their own PID namespace");
      assert.equal(values.get("CONTROL_SOCKET"), "hidden");
      assert.equal(values.get("GATEWAY_DIR"), "masked");
      assert.equal(values.get("HOST_TMP"), "hidden");
      assert.equal(values.get("WORKDIR"), "visible");
      if (mode === "publish") {
        assert.equal(values.get("HOME"), fixture.hostHome);
      } else {
        assert.match(values.get("HOME") ?? "", /^\/tmp\/pi-command-home-/);
      }
    });
  });
}

for (const mode of READ_ONLY_MODES) {
  test(`${mode} denies GitHub writes at the gateway and keeps the mode`, { timeout: 60_000 }, async () => {
    await fixture.withSession(mode, async (session) => {
      const rest = await session.bash(
        "curl -sS -D - -X POST -H 'Content-Type: application/json' -d '{}' https://api.github.com/user/repos -w '\\nHTTP_CODE=%{http_code}\\n'",
      );
      assert.equal(rest.status, 0, rest.output);
      assert.match(rest.output, /^HTTP\/1\.1 403 /m);
      assert.match(rest.output, /^X-Pi-Square-Denial: write_requires_publish\r?$/m);
      assert.match(rest.output, /^HTTP_CODE=403$/m);
      const body = JSON.parse(rest.output.split("\n").find((line) => line.startsWith("{")) ?? "null") as {
        message?: string; error?: { code?: string; message?: string };
      };
      assert.equal(body.error?.code, "write_requires_publish");
      assert.equal(body.message, `write_requires_publish: ${body.error?.message}`);

      const mutation = await session.bash("GH_NO_UPDATE_NOTIFIER=1 gh api graphql -f query='mutation{__typename}'");
      assert.equal(mutation.status, 1, mutation.output);
      assert.match(mutation.output, /gh: write_requires_publish: GraphQL mutations require publish mode \(HTTP 403\)$/m);

      const post = await session.bash("GH_NO_UPDATE_NOTIFIER=1 gh api -X POST /user/repos -f name=x");
      assert.equal(post.status, 1, post.output);
      assert.match(post.output, /gh: write_requires_publish: POST is a write and requires publish mode \(HTTP 403\)$/m);

      // Interactive denials never open the escalation dialog; the mode stays.
      assert.doesNotMatch(session.screen(), /Switch mode to publish/);
      await session.slash("/gh-mode status", `GitHub mode: ${mode}`);
    });
  });
}

test("commands reach nothing but the gateway, which accepts only public HTTPS CONNECT", { timeout: 60_000 }, async () => {
  await fixture.withSession("browse", async (session) => {
    const probe = [
      `probe() { local label=$1; shift; local out; out=$("$@" 2>&1); printf '%s=exit %s: %s\\n' "$label" "$?" "$out"; }`,
      `probe DIRECT curl -sS -m 5 --noproxy '*' https://api.github.com/`,
      `probe PLAIN_HTTP curl -sS -m 5 http://example.com/`,
      `probe PRIVATE curl -sS -m 5 https://10.0.0.1/`,
      `probe LOOPBACK curl -sS -m 5 https://127.0.0.1/`,
      `probe OTHER_PORT curl -sS -m 5 https://example.com:8443/`,
      `probe API_PORT curl -sS -m 5 https://api.github.com:8443/`,
    ].join("; ");
    const result = await session.bash(probe);
    assert.equal(result.status, 0, result.output);
    const values = parseLines(result.output);
    assert.match(values.get("DIRECT") ?? "", /^exit 6: curl: \(6\) Could not resolve host/);
    assert.equal(values.get("PLAIN_HTTP"), "exit 0: only CONNECT is supported");
    for (const label of ["PRIVATE", "LOOPBACK", "OTHER_PORT", "API_PORT"]) {
      // curl versions report a rejected CONNECT as either connect or receive failure.
      assert.match(values.get(label) ?? "", /^exit (7|56): curl: \(\1\) CONNECT tunnel failed, response 403$/, label);
    }
  });
});
