import { spawn, type ChildProcessByStdio } from "node:child_process";
import { randomBytes } from "node:crypto";
import type { Readable, Writable } from "node:stream";
import { setTimeout as delay } from "node:timers/promises";
import xterm from "@xterm/headless";
import type { BashCapture, Mode, ProcessResult } from "./types.js";

const TRUNCATION_NOTICE = "Output truncated. Full output:";
const TOOL_OUTPUT_STATUS = /Tool output: (expanded|collapsed)/g;

export function quote(value: string): string {
  return `'${value.replace(/'/g, `'"'"'`)}'`;
}

function count(text: string, needle: string): number {
  return text.split(needle).length - 1;
}

function toolOutputToggles(text: string): string {
  return (text.match(TOOL_OUTPUT_STATUS) ?? []).join(",");
}

export interface SessionOptions {
  binary: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
  mode: Mode;
  timeoutMs?: number;
}

export class SessionError extends Error {
  constructor(message: string, readonly screen: string, options?: ErrorOptions) {
    super(`${message}\nTerminal:\n${screen}`, options);
  }
}

// One real interactive pi under pi-square, driven through a controlling PTY.
// The screen is reconstructed from a headless terminal so wrapped lines and
// TUI redraws never confuse matching; typed text is matched against split
// markers so editor echoes cannot satisfy a result pattern.
export class PiSession {
  readonly started = Date.now();
  exitCode: number | null = null;
  signal: NodeJS.Signals | null = null;
  stderr = "";
  timedOut = false;

  private readonly deadline: number;
  private readonly mode: Mode;
  private readonly terminal = new xterm.Terminal({ cols: 240, rows: 80, scrollback: 10_000, allowProposedApi: true });
  private readonly child: ChildProcessByStdio<Writable, Readable, Readable>;
  private readonly finished: Promise<void>;
  private readonly reply: { dispose(): void };
  private pending = Promise.resolve();
  private closed = false;

  private constructor(options: SessionOptions) {
    this.deadline = this.started + (options.timeoutMs ?? 75_000);
    this.mode = options.mode;
    // util-linux script supplies a real controlling PTY without a native npm addon.
    const invocation = [options.binary, ...options.args].map(quote).join(" ");
    this.child = spawn("script", ["-qefc", `stty cols 240 rows 80; exec ${invocation}`, "/dev/null"], {
      cwd: options.cwd, env: { ...options.env, TERM: "xterm-256color" },
      detached: true, stdio: ["pipe", "pipe", "pipe"],
    });
    this.child.stdout.on("data", (data: Buffer) => {
      this.pending = this.pending.then(() => new Promise<void>((resolve) => this.terminal.write(data, resolve)));
    });
    this.child.stderr.on("data", (data) => { this.stderr += data.toString(); });
    this.child.on("error", (error) => { this.stderr += error.message; this.closed = true; });
    this.child.stdin.on("error", (error) => { this.stderr += error.message; });
    this.finished = new Promise<void>((resolve) => this.child.once("close", (code, sig) => {
      this.closed = true; this.exitCode = code; this.signal = sig; resolve();
    }));
    // Reply to terminal queries, as a real terminal would.
    this.reply = this.terminal.onData((data) => { if (!this.closed) this.child.stdin.write(data); });
  }

  static spawn(options: SessionOptions): PiSession {
    return new PiSession(options);
  }

  // Spawns pi and waits until it accepts input. The caller owns dispose().
  static async open(options: SessionOptions): Promise<PiSession> {
    const session = PiSession.spawn(options);
    try {
      await session.ready();
    } catch (error) {
      await session.dispose();
      throw error;
    }
    return session;
  }

  get elapsedMs(): number {
    return Date.now() - this.started;
  }

  screen(): string {
    const buffer = this.terminal.buffer.active;
    let text = "";
    for (let i = 0; i < buffer.length; i++) {
      const line = buffer.getLine(i)!;
      text += (line.isWrapped ? "" : "\n") + line.translateToString(true);
    }
    return text;
  }

  async ready(): Promise<void> {
    const modeWord = new RegExp(`\\b${this.mode}\\b`);
    await this.waitFor(() => {
      const text = this.screen();
      return text.includes("no-model-guard") && modeWord.test(text);
    }, "startup and model tripwire");
    // Confirm that pi is accepting input, not merely painting its initial UI.
    await this.slash("/gh-mode status", `GitHub mode: ${this.mode}`);
  }

  // Types a slash command and waits for one more occurrence of the expected
  // notification. pi rewrites the previous status line in place when nothing
  // was rendered in between, so consecutive slash commands must expect
  // different text.
  async slash(command: string, expect: string): Promise<void> {
    const before = count(this.screen(), expect);
    this.child.stdin.write(`${command}\r`);
    await this.waitFor(() => count(this.screen(), expect) > before, command);
  }

  async bash(command: string, prefix: "!" | "!!" = "!"): Promise<BashCapture> {
    return this.startBash(command, prefix).result;
  }

  // Runs the command inside the sandbox and captures its combined output and
  // exit status losslessly: the wrapper frames both as base64 between markers
  // that the echoed command text cannot reproduce because they are assembled
  // by printf from split arguments.
  startBash(command: string, prefix: "!" | "!!" = "!"): { completed(): boolean; result: Promise<BashCapture> } {
    const nonce = randomBytes(12).toString("hex");
    const shell = `f=$(mktemp) || exit; bash -c ${quote(command)} >"$f" 2>&1; r=$?; printf '\\n%s%s:%s:' PSQ_ ${nonce} "$r"; base64 -w0 "$f"; printf '\\n%s%s\\n' :END ${nonce}; rm -f "$f"; exit "$r"`;
    const done = new RegExp(`:END${nonce}[ \\t]*(\\n|$)`);
    const frame = new RegExp(`PSQ_${nonce}:(\\d+):([A-Za-z0-9+/=\\s]*):END${nonce}`);
    const truncationsBefore = count(this.screen(), TRUNCATION_NOTICE);
    this.child.stdin.write(`\x1b[200~${prefix} ${shell}\x1b[201~`);
    this.child.stdin.write("\r");
    const completed = () => done.test(this.screen());
    const capture = (): BashCapture | undefined => {
      const match = this.screen().match(frame);
      if (!match) return undefined;
      return { status: Number(match[1]), output: Buffer.from(match[2].replace(/\s/g, ""), "base64").toString("utf8") };
    };
    const result = (async () => {
      await this.waitFor(() => {
        if (count(this.screen(), TRUNCATION_NOTICE) > truncationsBefore) {
          throw this.error("command output exceeded pi's truncation limits and cannot be captured");
        }
        return completed();
      }, "bash output");
      // The completed component shows only its last screenful until expanded.
      // Ctrl+O toggles every component, so a second press may be needed when
      // an earlier command in this session already switched the state.
      let captured = capture();
      for (let toggles = 0; captured === undefined && toggles < 2; toggles++) {
        const before = toolOutputToggles(this.screen());
        this.child.stdin.write("\x0f");
        await this.waitFor(() => {
          captured = capture();
          return captured !== undefined || toolOutputToggles(this.screen()) !== before;
        }, "bash completion");
      }
      if (captured === undefined) throw this.error("bash result frame is not visible after expanding tool output");
      return captured;
    })();
    return { completed, result };
  }

  // /quit is handled by the real interactive UI, so shell handling settles
  // before exit and no test-only exit path exists.
  async quit(): Promise<void> {
    this.child.stdin.write("/quit\r");
    await this.waitFor(() => this.closed, "clean exit");
    if (this.exitCode !== 0) throw this.error(`pi exited with status ${this.exitCode}`);
  }

  async dispose(): Promise<void> {
    if (!this.closed) {
      try { process.kill(-this.child.pid!, "SIGKILL"); } catch { this.child.kill("SIGKILL"); }
    }
    await this.finished;
    await this.pending;
    this.reply.dispose();
    this.terminal.dispose();
  }

  error(message: string, cause?: unknown): SessionError {
    return new SessionError(message, this.screen(), cause === undefined ? undefined : { cause });
  }

  private async waitFor(condition: () => boolean, label: string): Promise<void> {
    while (true) {
      await this.pending;
      if (condition()) return;
      if (this.closed) throw this.error(`pi exited during ${label} (status ${this.exitCode})`);
      if (Date.now() >= this.deadline) {
        this.timedOut = true;
        throw this.error(`deadline exceeded during ${label}`);
      }
      await delay(25);
    }
  }
}

export async function runInteractive(options: SessionOptions & { command: string; prefix?: "!" | "!!" }): Promise<ProcessResult> {
  const prefix = options.prefix ?? "!";
  const session = PiSession.spawn(options);
  let status: number | null = null;
  let output = "";
  try {
    await session.ready();
    const capture = await session.bash(options.command, prefix);
    status = capture.status;
    output = capture.output;
    await session.quit();
  } catch (error) {
    status = null;
    session.stderr += `\n${error instanceof Error ? error.message : String(error)}`;
  } finally {
    await session.dispose();
  }
  return {
    mode: options.mode, command: `${prefix} ${options.command}`, status, signal: session.signal,
    stdout: output, stderr: session.stderr, timedOut: session.timedOut, elapsedMs: session.elapsedMs,
  };
}
