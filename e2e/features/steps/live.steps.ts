import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { Given, Then, When } from "@cucumber/cucumber";
import { findIssues, formatResult, ghApi, REPOSITORY, runSandbox } from "../../src/harness.js";
import type { Mode } from "../../src/types.js";
import { LiveWorld } from "../support/world.js";

function quote(value: string): string {
  return `'${value.replace(/'/g, `'"'"'`)}'`;
}

function requireMode(value: string): Mode {
  assert.ok(value === "browse" || value === "local" || value === "publish", `invalid mode ${value}`);
  return value;
}

Given("a unique GitHub issue for mode {string}", async function (this: LiveWorld, rawMode: string) {
  this.mode = requireMode(rawMode);
  const id = `pi-square-e2e-issue-${this.mode}-${randomUUID()}`;
  this.id = id;
  this.issue = {
    id,
    title: `[${id}] live gateway smoke test`,
    body: `Append-only live test artifact. Identifier: ${id}. Mode: ${this.mode}.`,
  };
  assert.deepEqual(await findIssues(id), [], `identifier collision before scenario: ${id}`);
});

When("the sandbox creates the GitHub issue", async function (this: LiveWorld) {
  assert.ok(this.mode && this.issue);
  const command = [
    "gh issue create",
    `--repo ${quote(REPOSITORY)}`,
    `--title ${quote(this.issue.title)}`,
    `--body ${quote(this.issue.body)}`,
  ].join(" ");
  this.result = await runSandbox(this.mode, command);
});

Then("issue creation is {string}", async function (this: LiveWorld, expected: string) {
  assert.ok(this.mode && this.issue && this.result);
  const diagnostic = formatResult(this.result);
  assert.equal(this.result.timedOut, false, diagnostic);
  assert.notEqual(this.result.status, null, `interactive session failed\n${diagnostic}`);
  if (expected === "denied") {
    assert.notEqual(this.result.status, 0, `denied command unexpectedly succeeded\n${diagnostic}`);
    assert.match(`${this.result.stdout}\n${this.result.stderr}`, /write_requires_publish/, `missing specific gateway denial\n${diagnostic}`);
    const unexpected = await findIssues(this.issue.id);
    assert.deepEqual(unexpected, [], `denied operation created append-only issue(s): ${unexpected.map((issue) => issue.html_url).join(", ")}\n${diagnostic}`);
    return;
  }

  assert.equal(expected, "allowed");
  assert.equal(this.result.status, 0, `publish command failed\n${diagnostic}`);
  const urls = `${this.result.stdout}\n${this.result.stderr}`.match(/https:\/\/github\.com\/[^\s]+\/issues\/\d+/g) ?? [];
  assert.equal(urls.length, 1, `expected one created issue URL\n${diagnostic}`);
  this.issueUrl = urls[0];
  const match = this.issueUrl.match(new RegExp(`^https://github\\.com/${REPOSITORY.replace("/", "\\/")}\/issues\/(\\d+)$`));
  assert.ok(match, `issue URL belongs to the wrong repository: ${this.issueUrl}`);
  const remote = await ghApi(`repos/${REPOSITORY}/issues/${match[1]}`) as Record<string, unknown>;
  assert.equal(remote.title, this.issue.title);
  assert.equal(remote.body, this.issue.body);
  assert.equal(remote.state, "open");
  assert.equal((remote.repository_url as string).endsWith(`/repos/${REPOSITORY}`), true);
  console.log(`Retained append-only issue: ${this.issueUrl}`);
});

Given("a unique HTTPS request for mode {string}", function (this: LiveWorld, rawMode: string) {
  this.mode = requireMode(rawMode);
  this.id = `pi-square-e2e-http-${this.mode}-${randomUUID()}`;
});

When("the sandbox GETs httpbin", async function (this: LiveWorld) {
  assert.ok(this.mode && this.id);
  const url = `https://httpbin.org/get?pi_square_id=${encodeURIComponent(this.id)}`;
  this.result = await runSandbox(this.mode, `curl --connect-timeout 10 --max-time 30 --fail-with-body --silent --show-error ${quote(url)} --write-out '\\n__PI_SQUARE_HTTP_STATUS__:%{http_code}\\n'`);
});

Then("the HTTPS response is successful", function (this: LiveWorld) {
  assert.ok(this.result && this.id);
  const diagnostic = formatResult(this.result);
  assert.equal(this.result.status, 0, `HTTPS command failed\n${diagnostic}`);
  // The PTY frame contains the command's combined stdout/stderr, without UI echoes.
  const output = this.result.stdout;
  const statusMarker = "__PI_SQUARE_HTTP_STATUS__:200";
  const markerIndex = output.lastIndexOf(statusMarker);
  assert.notEqual(markerIndex, -1, `HTTP status was not 200\n${diagnostic}`);
  const beforeMarker = output.slice(0, markerIndex);
  const jsonStart = beforeMarker.indexOf("{");
  assert.notEqual(jsonStart, -1, `httpbin JSON response was missing\n${diagnostic}`);
  const response = JSON.parse(beforeMarker.slice(jsonStart).trim()) as { args?: { pi_square_id?: string } };
  assert.equal(response.args?.pi_square_id, this.id, `httpbin did not echo the unique value\n${diagnostic}`);
});
