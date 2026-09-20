import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { LocalFixture, MODES } from "./local-fixture.js";

let fixture: LocalFixture;
before(async () => { fixture = await LocalFixture.create(); }, { timeout: 180_000 });
after(async () => { await fixture?.dispose(); });

test("interactive ! and !! route through the production sandbox", { timeout: 180_000 }, async () => {
  for (const mode of MODES) {
    const result = await fixture.run(mode,
      "printf 'hello\\n'; printf 'stderr\\n' >&2; if touch protected 2>/dev/null; then echo writable; else echo readonly; fi; exit 7",
      { prefix: mode === "local" ? "!!" : "!", timeoutMs: 40_000 });
    assert.equal(result.status, 7, JSON.stringify(result));
    assert.equal(result.timedOut, false);
    assert.equal(result.stdout, `hello\nstderr\n${mode === "browse" ? "readonly" : "writable"}\n`);
  }
});

test("capture is lossless for wrapped, binary, empty, and signal-terminated output", { timeout: 180_000 }, async () => {
  await fixture.withSession("browse", async (session) => {
    const wrapped = await session.bash("printf '%04000d\\n' 0; printf '\\033[31mred\\033[0m\\n'");
    assert.equal(wrapped.status, 0);
    assert.equal(wrapped.output, `${"0".repeat(4000)}\n\x1b[31mred\x1b[0m\n`);

    const binary = await session.bash("head -c 300 /dev/urandom | base64 -w0; printf '\\0\\1\\2\\n'");
    assert.equal(binary.status, 0);
    assert.match(binary.output.slice(0, 400), /^[A-Za-z0-9+/=]{400}$/);
    assert.equal(binary.output.slice(400), "\u0000\u0001\u0002\n");

    const empty = await session.bash("true");
    assert.deepEqual(empty, { status: 0, output: "" });

    const killed = await session.bash("kill -9 $$");
    assert.equal(killed.status, 137);
  });
});

test("a hung command fails the deadline without a false result", { timeout: 60_000 }, async () => {
  const timeout = await fixture.run("browse", "sleep 30", { timeoutMs: 2_000 });
  assert.equal(timeout.timedOut, true, JSON.stringify(timeout));
  assert.equal(timeout.status, null);
  assert.match(timeout.stderr, /deadline exceeded during bash output/);
});
