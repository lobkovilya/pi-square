import { spawn } from "node:child_process";
import { constants } from "node:fs";
import { access, mkdir, mkdtemp, open, readFile, realpath, rename, rm, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import type { Mode, ProcessResult } from "./types.js";
import { runInteractive } from "./terminal.js";

const configuredRepository = process.env.PI_SQUARE_E2E_REPOSITORY?.trim();
if (!configuredRepository) throw new Error("PI_SQUARE_E2E_REPOSITORY is required (OWNER/REPOSITORY)");
export const REPOSITORY = configuredRepository;
const EXPECTED_REMOTE = `https://github.com/${REPOSITORY}.git`;
const PROJECT_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const DEFAULT_BINARY = path.join(PROJECT_ROOT, "e2e/.tmp/pi-square");
const MARKER_SUFFIX = ".pi-square-e2e-owner.json";

let token = "";
let lockPath = "";
let checkout = "";
let binary = "";

function nonSecretEnvironment(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  delete env.PI_SQUARE_E2E_GITHUB_TOKEN;
  for (const key of ["GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST"]) delete env[key];
  return env;
}

function cleanHostEnvironment(): NodeJS.ProcessEnv {
  const env = nonSecretEnvironment();
  env.GH_TOKEN = token;
  return env;
}

export function redact(value: string): string {
  const secret = token || process.env.PI_SQUARE_E2E_GITHUB_TOKEN || "";
  return secret ? value.split(secret).join("[REDACTED]") : value;
}

export async function runProcess(
  command: string,
  args: string[],
  options: { cwd?: string; env?: NodeJS.ProcessEnv; timeoutMs?: number; mode?: Mode } = {},
): Promise<ProcessResult> {
  const started = Date.now();
  const child = spawn(command, args, {
    cwd: options.cwd,
    env: options.env ?? nonSecretEnvironment(),
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.setEncoding("utf8").on("data", (chunk) => { stdout += chunk; });
  child.stderr.setEncoding("utf8").on("data", (chunk) => { stderr += chunk; });
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    try { process.kill(-child.pid!, "SIGKILL"); } catch { child.kill("SIGKILL"); }
  }, options.timeoutMs ?? 60_000);
  const { code, signal } = await new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => resolve({ code, signal }));
  }).finally(() => clearTimeout(timer));
  return {
    mode: options.mode,
    command: [command, ...args].join(" "),
    status: code,
    signal,
    stdout: redact(stdout),
    stderr: redact(stderr),
    elapsedMs: Date.now() - started,
    timedOut,
  };
}

function failure(label: string, result: ProcessResult): Error {
  return new Error(`${label} failed\n${formatResult(result)}`);
}

export function formatResult(result: ProcessResult): string {
  return [
    `mode=${result.mode ?? "host"} status=${result.status} signal=${result.signal ?? "none"} elapsedMs=${result.elapsedMs} timedOut=${result.timedOut}`,
    `command=${redact(result.command)}`,
    `stdout:\n${result.stdout}`,
    `stderr:\n${result.stderr}`,
  ].join("\n");
}

async function requireTool(name: string): Promise<void> {
  const result = await runProcess("bash", ["-lc", `command -v -- ${name}`], { timeoutMs: 5_000 });
  if (result.status !== 0) throw failure(`required tool ${name}`, result);
}

async function gh(args: string[], timeoutMs = 30_000): Promise<ProcessResult> {
  return runProcess("gh", args, { env: cleanHostEnvironment(), timeoutMs });
}

function normalizedRemote(value: string): string {
  return value.trim().replace(/\/$/, "").replace(/^git@github\.com:/, "https://github.com/").replace(/\.git$/, "") + ".git";
}

async function acquireCheckoutLock(target: string): Promise<void> {
  lockPath = `${target}.lock`;
  await mkdir(path.dirname(target), { recursive: true });
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      const handle = await open(lockPath, "wx", 0o600);
      await handle.writeFile(`${process.pid}\n`);
      await handle.close();
      return;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      const owner = Number.parseInt((await readFile(lockPath, "utf8").catch(() => "")).trim(), 10);
      let alive = Number.isInteger(owner) && owner > 0;
      if (alive) {
        try { process.kill(owner, 0); } catch { alive = false; }
      }
      if (!alive && attempt === 0) {
        await rm(lockPath, { force: true });
        continue;
      }
      throw new Error(`checkout lock ${lockPath} is held by live process ${owner || "unknown"}; concurrent live runs must use separate PI_SQUARE_E2E_CHECKOUT paths`);
    }
  }
}

async function validateCheckout(target: string): Promise<void> {
  const marker = JSON.parse(await readFile(`${target}${MARKER_SUFFIX}`, "utf8")) as { repository?: string };
  if (marker.repository !== REPOSITORY) throw new Error(`refusing to reset ${target}: invalid test-ownership marker`);
  const top = await runProcess("git", ["rev-parse", "--show-toplevel"], { cwd: target, timeoutMs: 10_000 });
  if (top.status !== 0 || path.resolve(top.stdout.trim()) !== path.resolve(await realpath(target))) {
    throw new Error(`refusing to reset ${target}: it is not the expected repository root`);
  }
  const remote = await runProcess("git", ["remote", "get-url", "origin"], { cwd: target, timeoutMs: 10_000 });
  if (remote.status !== 0 || normalizedRemote(remote.stdout) !== EXPECTED_REMOTE) {
    throw new Error(`refusing to reset ${target}: origin is ${remote.stdout.trim() || "missing"}, expected ${EXPECTED_REMOTE}`);
  }
}

async function prepareCheckout(): Promise<void> {
  checkout = path.resolve(process.env.PI_SQUARE_E2E_CHECKOUT ?? path.join(process.env.XDG_CACHE_HOME ?? path.join(homedir(), ".cache"), "pi-square/e2e/checkout"));
  await acquireCheckoutLock(checkout);
  let exists = true;
  try { await access(checkout, constants.F_OK); } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    exists = false;
  }
  if (exists) {
    await validateCheckout(checkout);
  } else {
    const temp = `${checkout}.clone-${process.pid}`;
    await rm(temp, { recursive: true, force: true });
    const clone = await gh(["repo", "clone", REPOSITORY, temp, "--", "--quiet"], 60_000);
    if (clone.status !== 0) throw failure("fixture clone", clone);
    await rename(temp, checkout);
    await writeFile(`${checkout}${MARKER_SUFFIX}`, JSON.stringify({ repository: REPOSITORY }) + "\n", { mode: 0o600 });
    await validateCheckout(checkout);
  }
  for (const args of [
    ["fetch", "--prune", "origin", "main"],
    ["reset", "--hard", "origin/main"],
    ["clean", "-ffdx"],
  ]) {
    let env: NodeJS.ProcessEnv | undefined;
    if (args[0] === "fetch") {
      env = cleanHostEnvironment();
      env.GIT_CONFIG_COUNT = "1";
      env.GIT_CONFIG_KEY_0 = "credential.helper";
      env.GIT_CONFIG_VALUE_0 = "!f() { echo username=x-access-token; echo password=$GH_TOKEN; }; f";
    }
    const result = await runProcess("git", args, { cwd: checkout, env, timeoutMs: 60_000 });
    if (result.status !== 0) throw failure(`git ${args[0]}`, result);
  }
}

async function validateFixture(): Promise<void> {
  // Use REST rather than `gh repo view`: fine-grained repository PATs can read
  // this metadata but may not be allowed to resolve defaultBranchRef via GraphQL.
  const repo = await gh(["api", `repos/${REPOSITORY}`]);
  if (repo.status !== 0) throw failure("fixture repository authentication", repo);
  const metadata = JSON.parse(repo.stdout) as { private: boolean; has_issues: boolean; default_branch?: string };
  if (!metadata.private || !metadata.has_issues || metadata.default_branch !== "main") {
    throw new Error(`fixture ${REPOSITORY} must be private, have issues enabled, and use main`);
  }
  const readme = await gh(["api", `repos/${REPOSITORY}/contents/README.md`, "-H", "Accept: application/vnd.github.raw+json"]);
  if (readme.status !== 0) throw failure("fixture README", readme);
  if (readme.stdout.trimEnd() !== "Private fixture repository for pi-square sandbox integration tests.") {
    throw new Error(`fixture ${REPOSITORY} has unexpected README.md content`);
  }
}

async function prepareBinary(): Promise<void> {
  const selected = process.env.PI_SQUARE_E2E_EXECUTABLE;
  if (selected) {
    binary = path.resolve(selected);
    await access(binary, constants.X_OK);
    return;
  }
  binary = DEFAULT_BINARY;
  await mkdir(path.dirname(binary), { recursive: true });
  const build = await runProcess("go", ["build", "-o", binary, "."], { cwd: PROJECT_ROOT, timeoutMs: 120_000 });
  if (build.status !== 0) throw failure("pi-square build", build);
}

export async function runSandbox(mode: Mode, shellCommand: string, prefix: "!" | "!!" = "!"): Promise<ProcessResult> {
  // ~/.pi is a production writable mount; host /tmp is intentionally hidden.
  const base = path.join(homedir(), ".pi", "e2e");
  await mkdir(base, { recursive: true });
  const agentDir = await mkdtemp(path.join(base, "agent-"));
  try {
    const guard = path.join(agentDir, "no-model.ts");
    await writeFile(guard, await readFile(path.join(PROJECT_ROOT, "e2e/src/no-model.ts")));
    await writeFile(path.join(agentDir, "settings.json"), JSON.stringify({ quietStartup: true }));
    const piArgs = [
      "--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates",
      "--no-themes", "--no-context-files", "--no-approve", "--offline", "--extension", guard,
    ];
    const result = await runInteractive({
      binary, args: [`--gh-mode=${mode}`, "--", ...piArgs], cwd: checkout,
      env: { ...cleanHostEnvironment(), PI_CODING_AGENT_DIR: agentDir },
      mode, command: shellCommand, prefix,
    });
    return { ...result, stdout: redact(result.stdout), stderr: redact(result.stderr) };
  } finally {
    await rm(agentDir, { recursive: true, force: true });
  }
}

export async function setupSuite(): Promise<void> {
  token = process.env.PI_SQUARE_E2E_GITHUB_TOKEN ?? "";
  if (!token) throw new Error("PI_SQUARE_E2E_GITHUB_TOKEN is required for live tests");
  await Promise.all(["go", "pi", "gh", "git", "curl", "script", "stty", "base64"].map(requireTool));
  await validateFixture();
  await prepareBinary();
  await prepareCheckout();
  for (const prefix of ["!", "!!"] as const) {
    const probe = await runSandbox("browse", "printf 'interactive-ready\\n'; exit 7", prefix);
    if (probe.status !== 7 || probe.stdout !== "interactive-ready\n" || probe.timedOut) {
      throw failure(`interactive ${prefix} preflight`, probe);
    }
  }
}

export async function teardownSuite(): Promise<void> {
  if (lockPath) await rm(lockPath, { force: true });
}

export async function ghApi(pathname: string): Promise<unknown> {
  const result = await gh(["api", pathname]);
  if (result.status !== 0) throw failure(`gh api ${pathname}`, result);
  return JSON.parse(result.stdout);
}

export async function findIssues(identifier: string): Promise<Array<Record<string, unknown>>> {
  const matches: Array<Record<string, unknown>> = [];
  for (let page = 1; page <= 20; page++) {
    const items = await ghApi(`repos/${REPOSITORY}/issues?state=all&per_page=100&page=${page}`) as Array<Record<string, unknown>>;
    matches.push(...items.filter((item) => item.title === identifier || String(item.body ?? "").includes(identifier)));
    if (items.length < 100 || matches.length > 0) break;
  }
  return matches;
}
