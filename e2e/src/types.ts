export type Mode = "browse" | "local" | "publish";

export interface BashCapture {
  status: number;
  output: string;
}

export interface ProcessResult {
  mode?: Mode;
  command: string;
  status: number | null;
  signal: NodeJS.Signals | null;
  stdout: string;
  stderr: string;
  elapsedMs: number;
  timedOut: boolean;
}

export interface IssueFixture {
  id: string;
  title: string;
  body: string;
}
