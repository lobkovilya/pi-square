import fs from "node:fs";
import path from "node:path";
import type { ExtensionAPI, ExtensionContext, ToolResultEvent } from "@earendil-works/pi-coding-agent";
import { Key } from "@earendil-works/pi-tui";

type GhMode = "browse" | "local" | "publish";

const STATE_ENTRY = "gh-mode-state";
const STATUS_KEY = "gh-mode";
const GITHUB_ICON = "";
const DENY_WRITE = "write_requires_publish";

// Restore browse or local from the session, but never silently re-enter
// publish: a resumed session starts read-only and the user re-enables writes
// deliberately.
function restoreMode(ctx: ExtensionContext): GhMode {
	let mode: GhMode = "browse";
	for (const entry of ctx.sessionManager.getEntries()) {
		if (entry.type === "custom" && entry.customType === STATE_ENTRY) {
			const restored = (entry.data as { mode?: GhMode } | undefined)?.mode;
			if (restored === "browse" || restored === "local") mode = restored;
			else if (restored === "publish") mode = "browse";
		}
	}
	return mode;
}

function modePrompt(mode: GhMode): string {
	const permission = mode === "browse"
		? "The entire workdir is read-only, and the gateway GitHub credential cannot perform remote writes."
		: mode === "local"
			? "Local Git changes are allowed; the gateway GitHub credential cannot perform remote writes."
			: "Local Git changes and remote GitHub writes using the gateway credential are allowed.";
	return [
		`GitHub safety mode: ${mode}. ${permission}`,
		"Shell commands can reach public HTTPS destinations on port 443 only through a mandatory proxy.",
		"For api.github.com, browse and local permit REST GET/HEAD and GraphQL queries using the gateway credential.",
		"New commands launched in publish may use the gateway credential for REST writes and GraphQL mutations.",
		"Other public HTTPS destinations, including other GitHub hosts, are raw end-to-end TLS tunnels and receive no gateway credential or policy enforcement.",
		"Independently available credentials are outside the GitHub safety-mode guarantee.",
		"Plain HTTP, non-HTTPS ports, SSH, direct networking, and proxy bypass do not work.",
		"Mode changes affect newly launched commands only; commands already running keep the permissions they launched with.",
		"If a task needs a write through api.github.com, attempt the command once; the mode guard offers to switch to publish. If the user keeps the current mode, do not repeat the blocked operation. The user can also switch with /gh-mode.",
	].join(" ");
}

function shellQuote(value: string): string {
	return `'${value.replace(/'/g, `'"'"'`)}'`;
}

// routeCommand rewrites a shell command so pi's bash tool execs the trusted
// launcher stub instead of the command itself. The command is base64-encoded so
// no quoting can leak out, and the mode selects the launch class. The stub, not
// this shell, reaches the supervisor.
function routeCommand(command: string, mode: GhMode): string {
	const helper = process.env.PI_SQUARE_GH_HELPER;
	if (!helper) {
		return "printf '%s\\n' 'pi-square launcher is not configured.' >&2; exit 127";
	}
	const encoded = Buffer.from(command, "utf8").toString("base64");
	return `exec ${shellQuote(helper)} --pi-square-stub ${mode} ${encoded}`;
}

function isInside(child: string, parent: string): boolean {
	const relative = path.relative(parent, child);
	return relative === "" || (!!relative && !relative.startsWith("..") && !path.isAbsolute(relative));
}

function resolvedTargetPath(inputPath: string): string {
	const absolute = path.resolve(process.cwd(), inputPath);
	if (fs.existsSync(absolute)) return fs.realpathSync.native(absolute);
	const parent = fs.realpathSync.native(path.dirname(absolute));
	return path.join(parent, path.basename(absolute));
}

export default function ghModeExtension(pi: ExtensionAPI): void {
	// The source lives in this repository, but the extension is enabled only by
	// the pi-square wrapper, which embeds it and sets this marker.
	if (process.env.PI_SQUARE_ACTIVE !== "1") return;

	let mode: GhMode = "browse";
	const workdir = fs.realpathSync.native(process.cwd());
	const launchedMode = new Map<string, GhMode>();

	function persistMode(): void {
		pi.appendEntry(STATE_ENTRY, { mode });
	}

	function updateStatus(ctx: ExtensionContext): void {
		const color = mode === "browse" ? "success" : mode === "local" ? "accent" : "warning";
		ctx.ui.setStatus(STATUS_KEY, ctx.ui.theme.fg(color, mode));
	}

	function setMode(next: GhMode, ctx: ExtensionContext, notify = true): void {
		mode = next;
		updateStatus(ctx);
		persistMode();
		if (notify) ctx.ui.notify(`${GITHUB_ICON} GitHub mode: ${mode}`, "info");
	}

	function modeIncludes(current: GhMode, required: GhMode): boolean {
		const rank: Record<GhMode, number> = { browse: 0, local: 1, publish: 2 };
		return rank[current] >= rank[required];
	}

	async function suggestMode(required: GhMode, ctx: ExtensionContext, reason: string): Promise<boolean> {
		if (modeIncludes(mode, required)) return true;
		if (!ctx.hasUI) return false;

		const previous = mode;
		const switchOption = `Switch mode to ${required}`;
		const choice = await ctx.ui.select(`${GITHUB_ICON} GitHub mode is ${previous}. ${reason}`, [
			switchOption,
			"Keep existing",
		]);
		if (choice !== switchOption) return false;

		// Another prompt or shortcut may have changed the mode while this dialog
		// was open. Never overwrite that newer choice unless it is insufficient.
		if (mode !== previous) return modeIncludes(mode, required);
		setMode(required, ctx);
		return true;
	}

	async function routeBash(event: { input: { command?: string }; toolName: string; toolCallId: string }) {
		if (event.toolName !== "bash" || typeof event.input.command !== "string") return;
		launchedMode.set(event.toolCallId, mode);
		event.input.command = routeCommand(event.input.command, mode);
	}

	async function guardWorkdirEdits(event: { input: { path?: string }; toolName: string }, ctx: ExtensionContext) {
		if (mode !== "browse" || !["edit", "write"].includes(event.toolName) || typeof event.input.path !== "string") return;
		const lexicalTarget = path.resolve(process.cwd(), event.input.path);
		let resolvedTarget: string;
		try {
			resolvedTarget = resolvedTargetPath(event.input.path);
		} catch {
			return { block: true, reason: `GitHub mode is browse: cannot resolve ${event.input.path} for workdir protection.` };
		}
		if (isInside(lexicalTarget, workdir) || isInside(resolvedTarget, workdir)) {
			if (await suggestMode("local", ctx, "Editing the workdir requires local mode.")) return;
			return { block: true, reason: "GitHub mode is browse: the workdir is read-only. The user chose to keep the existing mode." };
		}
	}

	async function offerEscalationOnDenial(event: ToolResultEvent, ctx: ExtensionContext) {
		launchedMode.delete(event.toolCallId);
		if (event.toolName !== "bash") return;
		const text = event.content.map((part) => (part.type === "text" ? part.text : "")).join("\n");
		if (!text.includes(DENY_WRITE)) return;
		if (modeIncludes(mode, "publish")) return;

		const changed = await suggestMode("publish", ctx, "A command was denied because it writes to GitHub.");
		const hint = changed
			? `${GITHUB_ICON} GitHub mode switched to publish. Launch the command again to perform the write.`
			: `${GITHUB_ICON} GitHub mode remains ${mode}. Writes require publish mode (/gh-mode publish).`;
		return { content: [...event.content, { type: "text" as const, text: hint }] };
	}

	function nextMode(): GhMode {
		if (mode === "browse") return "local";
		if (mode === "local") return "publish";
		return "browse";
	}

	pi.registerCommand("gh-mode", {
		description: "Show or switch GitHub/git mode: /gh-mode, /gh-mode browse, /gh-mode local, /gh-mode publish, /gh-mode toggle",
		handler: async (args, ctx) => {
			const arg = args.trim().toLowerCase();
			if (!arg || arg === "status") {
				updateStatus(ctx);
				ctx.ui.notify(`${GITHUB_ICON} GitHub mode: ${mode}\nShell commands can reach public HTTPS destinations on port 443 through the gateway; only api.github.com receives the gateway credential.`, "info");
				return;
			}
			if (arg === "browse" || arg === "local" || arg === "publish") return setMode(arg, ctx);
			if (arg === "toggle" || arg === "t") return setMode(nextMode(), ctx);
			ctx.ui.notify("Usage: /gh-mode [browse|local|publish|toggle|status]", "error");
		},
	});

	pi.registerShortcut(Key.altSuper("g"), {
		description: "Toggle GitHub mode",
		handler: async (ctx) => setMode(nextMode(), ctx),
	});

	pi.on("session_start", async (_event, ctx) => {
		mode = restoreMode(ctx);
		updateStatus(ctx);
	});

	if (process.env.PI_SQUARE_DEV_MODE !== "1") {
		pi.on("before_agent_start", async (event) => ({
			systemPrompt: `${event.systemPrompt}\n\n${modePrompt(mode)}`,
		}));
	}

	pi.on("tool_call", guardWorkdirEdits);
	pi.on("tool_call", routeBash);
	pi.on("tool_result", offerEscalationOnDenial);
}
