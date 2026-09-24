// Safety tripwire only: never routes or executes shell commands. Interactive
// ! input bypasses agent input; any accidental prompt must fail before a call.
export default function (pi: any): void {
  const fail = () => {
    process.stderr.write("E2E: unexpected model input\n");
    process.exit(90);
  };
  pi.on("session_start", (_event: unknown, ctx: any) => {
    ctx.ui.setStatus("e2e-no-model", "no-model-guard");
  });
  pi.on("input", fail);
  pi.on("before_agent_start", fail);
  pi.on("before_provider_request", fail);
}
