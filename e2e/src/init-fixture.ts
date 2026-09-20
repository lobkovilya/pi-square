import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { REPOSITORY, runProcess } from "./harness.js";

const confirmation = `--confirm=${REPOSITORY}`;
if (!process.argv.includes(confirmation)) {
  throw new Error(`repository creation is irreversible from this script; confirm the supplied spelling with: npm run fixture:init -- ${confirmation}`);
}
const token = process.env.PI_SQUARE_E2E_GITHUB_TOKEN;
const env: NodeJS.ProcessEnv = { ...process.env };
delete env.PI_SQUARE_E2E_GITHUB_TOKEN;
if (token) {
  env.GH_TOKEN = token;
  for (const key of ["GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST"]) delete env[key];
}

async function checked(command: string, args: string[], cwd?: string): Promise<void> {
  const result = await runProcess(command, args, { cwd, env, timeoutMs: 60_000 });
  if (result.status !== 0) throw new Error(`${command} ${args.join(" ")} failed: ${result.stderr}`);
}

const existing = await runProcess("gh", ["repo", "view", REPOSITORY], { env, timeoutMs: 20_000 });
if (existing.status === 0) throw new Error(`${REPOSITORY} already exists; refusing to alter or recreate it`);

const dir = await mkdtemp(path.join(tmpdir(), "pi-square-e2e-fixture-"));
try {
  await writeFile(path.join(dir, "README.md"), "Private fixture repository for pi-square sandbox integration tests.\n");
  await checked("git", ["init", "-b", "main"], dir);
  await checked("git", ["add", "README.md"], dir);
  await checked("git", ["-c", "user.name=pi-square e2e", "-c", "user.email=pi-square-e2e@users.noreply.github.com", "commit", "-m", "Initialize live test fixture"], dir);
  await checked("gh", ["repo", "create", REPOSITORY, "--private", "--source", dir, "--remote", "origin", "--push"], dir);
  await checked("gh", ["api", "--method", "PATCH", `repos/${REPOSITORY}`, "-F", "has_issues=true"]);
  console.log(`Created private append-only fixture ${REPOSITORY}`);
} finally {
  await rm(dir, { recursive: true, force: true });
}
