import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { PiSession, SessionError, runInteractive } from "./terminal.js";
import type { Mode, ProcessResult } from "./types.js";

export const PROJECT_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
export const MODES: readonly Mode[] = ["browse", "local", "publish"];

// Never a real credential: pi-square resolves it as the host GitHub token, so
// it must stay inside the host gateway and out of every sandboxed command.
export const FAKE_HOST_TOKEN = "e2e-host-token-must-not-leak";
export const DUMMY_COMMAND_TOKEN = "pi-square-gateway-dummy";
export const PI_ARGS = [
  "--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates",
  "--no-themes", "--no-context-files", "--no-approve", "--offline",
];

// Real pi and the production sandbox with a dummy token: no GitHub or model
// calls, and no upstream network.
export class LocalFixture {
  private constructor(
    readonly binary: string,
    readonly workdir: string,
    readonly hostHome: string,
    readonly hostSecret: string,
    private readonly agentDir: string,
    private readonly buildDir: string,
  ) {}

  static async create(): Promise<LocalFixture> {
    const buildDir = await mkdtemp(path.join(tmpdir(), "pi-square-build-"));
    const workdir = await mkdtemp(path.join(tmpdir(), "pi-square-pty-"));
    const secretDir = await mkdtemp(path.join(tmpdir(), "pi-square-host-only-"));
    await mkdir(path.join(homedir(), ".pi"), { recursive: true });
    const agentDir = await mkdtemp(path.join(homedir(), ".pi", "pty-test-"));
    const binary = path.join(buildDir, "pi-square");
    await promisify(execFile)("go", ["build", "-o", binary, "."], { cwd: PROJECT_ROOT });
    const hostSecret = path.join(secretDir, "host-only-file");
    await writeFile(hostSecret, "must not be visible inside the sandbox\n");
    await writeFile(path.join(agentDir, "no-model.ts"), await readFile(path.join(PROJECT_ROOT, "e2e/src/no-model.ts")));
    await writeFile(path.join(agentDir, "settings.json"), '{"quietStartup":true}');
    return new LocalFixture(binary, workdir, await realpath(homedir()), hostSecret, agentDir, buildDir);
  }

  env(): NodeJS.ProcessEnv {
    const env: NodeJS.ProcessEnv = { ...process.env, GH_TOKEN: FAKE_HOST_TOKEN, PI_CODING_AGENT_DIR: this.agentDir };
    for (const key of ["PI_SQUARE_E2E_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST"]) delete env[key];
    return env;
  }

  // mode undefined starts pi-square without --gh-mode, which must yield browse.
  args(mode: Mode | undefined): string[] {
    const guard = path.join(this.agentDir, "no-model.ts");
    return [...(mode ? [`--gh-mode=${mode}`] : []), "--", ...PI_ARGS, "--extension", guard];
  }

  run(mode: Mode, command: string, options: { prefix?: "!" | "!!"; timeoutMs?: number } = {}): Promise<ProcessResult> {
    return runInteractive({
      binary: this.binary, cwd: this.workdir, env: this.env(), mode,
      args: this.args(mode), command, prefix: options.prefix, timeoutMs: options.timeoutMs,
    });
  }

  async withSession<T>(mode: Mode | undefined, body: (session: PiSession) => Promise<T>, timeoutMs = 90_000): Promise<T> {
    const session = PiSession.spawn({
      binary: this.binary, cwd: this.workdir, env: this.env(), mode: mode ?? "browse", args: this.args(mode), timeoutMs,
    });
    try {
      await session.ready();
      const value = await body(session);
      await session.quit();
      return value;
    } catch (error) {
      if (error instanceof SessionError) throw error;
      throw session.error(`${mode ?? "default"} session failed`, error);
    } finally {
      await session.dispose();
    }
  }

  async dispose(): Promise<void> {
    for (const dir of [this.agentDir, this.workdir, path.dirname(this.hostSecret), this.buildDir]) {
      await rm(dir, { recursive: true, force: true });
    }
  }
}
