import fs from "node:fs";
import path from "node:path";
import { createLocalBashOperations, type ExtensionAPI, type ExtensionContext, type ToolResultEvent } from "@earendil-works/pi-coding-agent";
import { Key } from "@earendil-works/pi-tui";

type Profile = { workdir: "ro" | "rw"; github: "ro" | "rw"; net: "on" | "off"; bash: "on" | "off" };
const RESOURCES = ["workdir", "github", "net", "bash"] as const;
const ICONS = ["", "", "", ""];
function shellQuote(value: string): string { return `'${value.replace(/'/g, `'"'"'`)}'`; }
function routeCommand(command: string, currentProfile: string): string {
 const helper = process.env.PI_SQUARE_GH_HELPER;
 if (!helper) throw new Error("pi-square launcher is not configured");
 return `exec ${shellQuote(helper)} --pi-square-stub ${shellQuote(currentProfile)} ${Buffer.from(command).toString("base64")}`;
}
function isInside(child: string, parent: string): boolean {
 const relative = path.relative(parent, child);
 return relative === "" || (relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative));
}
// Resolve the nearest existing ancestor, including for write's new directories.
function resolvedTargetPath(input: string): string {
 let current = path.resolve(input);
 const tail: string[] = [];
 while (!fs.existsSync(current)) {
  const parent = path.dirname(current);
  if (parent === current) throw new Error("cannot resolve path");
  tail.unshift(path.basename(current)); current = parent;
 }
 return path.join(fs.realpathSync.native(current), ...tail);
}
export default function permissionProfileExtension(pi: ExtensionAPI): void {
 if (process.env.PI_SQUARE_ACTIVE !== "1") return;
 const config = JSON.parse(process.env.PI_SQUARE_CONFIG!) as { defaultProfile: string; profiles: Record<string, Profile> };
 const profiles = config.profiles;
 const names = Object.keys(profiles);
 const hasProfile = (name: string) => Object.prototype.hasOwnProperty.call(profiles, name);
 let currentProfile = config.defaultProfile;
 const initial = process.env.PI_SQUARE_INITIAL_PROFILE;
 delete process.env.PI_SQUARE_INITIAL_PROFILE;
 let applyInitial = !!initial && hasProfile(initial);
 const workdir = fs.realpathSync.native(process.cwd());
 const increases = (from: string, to: string) => RESOURCES.some(key => profiles[from][key] !== profiles[to][key] && ["rw", "on"].includes(profiles[to][key]));
 function update(ctx: ExtensionContext): void {
  const p = profiles[currentProfile];
  const active = pi.getActiveTools();
  if ((p.bash === "on") !== active.includes("bash")) pi.setActiveTools(p.bash === "on" ? [...active, "bash"] : active.filter(n => n !== "bash"));
  ctx.ui.setStatus("pi-square-profile", ctx.ui.theme.fg(p.github === "rw" ? "warning" : "accent", [currentProfile, ...RESOURCES.map((key, i) => `${ICONS[i]} ${p[key]}`)].join(" · ")));
 }
 function switchProfile(next: string, ctx: ExtensionContext): boolean {
  currentProfile = next; update(ctx); pi.appendEntry("pi-square-profile-state", { profile: currentProfile }); ctx.ui.notify(`pi-square: profile ${currentProfile}`, "info");
  return true;
 }
 async function suggest(key: "workdir" | "github", ctx: ExtensionContext): Promise<boolean> {
  if (!ctx.hasUI) return false;
  const previous = currentProfile;
  const choices = names.filter(n => profiles[n][key] === "rw" && (key !== "github" || (profiles[n].net === "on" && profiles[n].bash === "on")));
  if (!choices.length) return false;
  const selected = await ctx.ui.select(`${key} write access requires another profile`, [...choices, "Keep existing"]);
  if (!selected || !choices.includes(selected) || currentProfile !== previous) return false;
  return switchProfile(selected, ctx);
 }
 const command = {
  description: `Show or switch permission profile: ${names.join(", ")}; toggle; status`,
  handler: async (args: string, ctx: ExtensionContext) => {
   const arg = args.trim();
   if (!arg || arg === "status") { update(ctx); ctx.ui.notify(`pi-square: profile ${currentProfile}. Profiles: ${names.join(", ")}`, "info"); return; }
   const next = arg === "toggle" || arg === "t" ? names[(names.indexOf(currentProfile) + 1) % names.length] : arg;
   if (!hasProfile(next)) { ctx.ui.notify(`Unknown profile. Choose: ${names.join(", ")}`, "error"); return; }
   await switchProfile(next, ctx);
  },
 };
 pi.registerCommand("pi-square-profile", command);
 pi.registerShortcut(Key.altSuper("g"), { description: "Cycle permission profiles", handler: async ctx => { await switchProfile(names[(names.indexOf(currentProfile) + 1) % names.length], ctx); } });
 pi.on("session_start", async (_event, ctx) => {
  currentProfile = config.defaultProfile;
  // Never restore an escalation over the configured default. Definitions may have changed.
  for (const entry of ctx.sessionManager.getEntries()) {
   if (entry.type !== "custom" || entry.customType !== "pi-square-profile-state") continue;
   const restored = (entry.data as { profile?: string })?.profile;
   currentProfile = restored && hasProfile(restored) && !increases(config.defaultProfile, restored) ? restored : config.defaultProfile;
  }
  if (applyInitial) { currentProfile = initial!; applyInitial = false; }
  update(ctx);
 });
 pi.on("user_bash", () => {
  const launchProfile = currentProfile;
  const local = createLocalBashOperations();
  return { operations: { exec(command, cwd, options) { return local.exec(routeCommand(command, launchProfile), cwd, options); } } };
 });
 pi.on("tool_call", async (event, ctx) => {
  if (event.toolName === "bash") {
   if (profiles[currentProfile].bash === "off") return { block: true, reason: "The bash tool is unavailable in this profile. Use /pi-square-profile to switch." };
   event.input.command = routeCommand(event.input.command as string, currentProfile);
  }
  if (profiles[currentProfile].workdir !== "ro" || !["edit", "write"].includes(event.toolName) || typeof event.input.path !== "string") return;
  try {
   if (!isInside(path.resolve(event.input.path), workdir) && !isInside(resolvedTargetPath(event.input.path), workdir)) return;
  } catch { return { block: true, reason: "Cannot resolve path for workdir protection." }; }
  if (await suggest("workdir", ctx)) return;
  return { block: true, reason: "The workdir is read-only in this profile." };
 });
 pi.on("tool_result", async (event: ToolResultEvent, ctx) => {
  if (event.toolName !== "bash" || profiles[currentProfile].github === "rw") return;
  if (!event.content.some(p => p.type === "text" && p.text.includes("write_requires_publish"))) return;
  const changed = await suggest("github", ctx);
  return { content: [...event.content, { type: "text" as const, text: changed ? `Switched to ${currentProfile}. Retry the command if still needed.` : `Profile remains ${currentProfile}; do not repeat the blocked write.` }] };
 });
 if (process.env.PI_SQUARE_DEV_MODE !== "1") pi.on("before_agent_start", async event => {
  const p = profiles[currentProfile];
  const prompt = [
   `Permission profile: ${currentProfile}. ${RESOURCES.map(k => `${k}: ${p[k]}`).join(", ")}.`,
   p.workdir === "ro" ? "Do not modify the workdir." : "Local edits and Git mutations are permitted.",
   p.github === "ro" ? "Keep work on this host; do not publish or upload work products, even through other credentials." : "GitHub publication is permitted when requested, not required.",
   `The bash tool is ${p.bash === "on" ? "available" : "unavailable"}.`,
   p.net === "off" ? "Sandboxed commands have no network access, including GitHub. Pi's provider connection is unaffected." : "Commands reach public HTTPS on port 443 only via the mandatory proxy; direct networking, HTTP, SSH and proxy bypass are blocked.",
   "The github permission governs gateway-authenticated API and remote Git operations: ro permits REST GET/HEAD, GraphQL queries and Git fetch; rw also permits REST writes, mutations and pushes. Other public HTTPS hosts are raw TLS tunnels; independently available credentials are outside this guarantee.",
   "Local Git follows filesystem permissions; there is no independent Git metadata protection. Permissions stay fixed for each command's lifetime. bash controls the model tool, not user !/!! commands.",
   "Use /pi-square-profile to switch immediately. Only attempt a blocked GitHub write when publication was explicitly requested; do not repeat it if the user keeps the profile.",
  ].join(" ");
  return { systemPrompt: `${event.systemPrompt}\n\n${prompt}` };
 });
}
