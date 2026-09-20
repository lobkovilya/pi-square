import { setWorldConstructor, World } from "@cucumber/cucumber";
import type { IssueFixture, Mode, ProcessResult } from "../../src/types.js";

export class LiveWorld extends World {
  mode?: Mode;
  id?: string;
  issue?: IssueFixture;
  result?: ProcessResult;
  issueUrl?: string;
}

setWorldConstructor(LiveWorld);
