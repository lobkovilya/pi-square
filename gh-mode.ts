import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { ExtensionAPI, ExtensionContext, ToolResultEvent } from "@earendil-works/pi-coding-agent";
import { Key } from "@earendil-works/pi-tui";

type GhMode = "browse" | "local" | "publish";

const STATE_ENTRY = "gh-mode-state";
const STATUS_KEY = "gh-mode";
const GITHUB_ICON = ""; // Nerd Font / Font Awesome GitHub mark
// Configure exactly these environment variables for GitHub tokens.
const RO_TOKEN_ENV = "PI_GH_RO_TOKEN";
const W_TOKEN_ENV = "PI_GH_W_TOKEN";

function restoreMode(ctx: ExtensionContext, fallback: GhMode): GhMode {
	let mode = fallback;
	for (const entry of ctx.sessionManager.getEntries()) {
		if (entry.type === "custom" && entry.customType === STATE_ENTRY) {
			const restored = (entry.data as { mode?: GhMode } | undefined)?.mode;
			if (restored === "browse" || restored === "local" || restored === "publish") mode = restored;
		}
	}
	return mode;
}

function tokenEnvName(mode: GhMode): string {
	return mode === "publish" ? W_TOKEN_ENV : RO_TOKEN_ENV;
}

function explicitGhTokenEnv(command: string): string | undefined {
	for (const token of tokenize(command)) {
		if (!token.startsWith("GH_TOKEN=")) continue;
		const value = token.slice("GH_TOKEN=".length);
		if (value === `$${RO_TOKEN_ENV}` || value === `\${${RO_TOKEN_ENV}}`) return RO_TOKEN_ENV;
		if (value === `$${W_TOKEN_ENV}` || value === `\${${W_TOKEN_ENV}}`) return W_TOKEN_ENV;
	}
	return undefined;
}

function prefixGhToken(command: string, mode: GhMode): string {
	const explicitTokenEnv = explicitGhTokenEnv(command);
	if (explicitTokenEnv) return `unset GH_TOKEN; ${command}`;
	const tokenEnv = tokenEnvName(mode);
	return `export GH_TOKEN="\${${tokenEnv}}"; ${command}`;
}

function prefixGitToken(command: string, mode: GhMode): string {
	const tokenEnv = tokenEnvName(mode);
	// GH_TOKEN is not used by git itself. Provide the selected token through
	// GIT_ASKPASS for HTTPS GitHub remotes, and reset credential.helper so a
	// stored write credential cannot bypass browse/local modes.
	const hideWriteToken = mode === "publish" ? "" : `unset ${W_TOKEN_ENV}; `;
	return `${hideWriteToken}tmp="$(mktemp)"; cat >"$tmp" <<'PI_GH_MODE_ASKPASS'
#!/bin/sh
case "$1" in
	*Username*) printf '%s\\n' 'x-access-token' ;;
	*Password*) printf '%s\\n' "$GH_TOKEN" ;;
	*) printf '\\n' ;;
esac
PI_GH_MODE_ASKPASS
chmod 700 "$tmp"; trap 'rm -f "$tmp"' EXIT; export GH_TOKEN="\${${tokenEnv}}" GIT_ASKPASS="$tmp" GIT_TERMINAL_PROMPT=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=; ${command}`;
}

function tokenize(command: string): string[] {
	const tokens: string[] = [];
	let current = "";
	let quote: "'" | '"' | undefined;
	let escaped = false;
	for (const ch of command) {
		if (escaped) {
			current += ch;
			escaped = false;
			continue;
		}
		if (ch === "\\" && quote !== "'") {
			escaped = true;
			continue;
		}
		if (quote) {
			if (ch === quote) quote = undefined;
			else current += ch;
			continue;
		}
		if (ch === "'" || ch === '"') {
			quote = ch;
			continue;
		}
		if (/\s/.test(ch) || "|&;()<>".includes(ch)) {
			if (current) tokens.push(current);
			current = "";
			continue;
		}
		current += ch;
	}
	if (current) tokens.push(current);
	return tokens;
}

function hasGhInvocation(command: string): boolean {
	return tokenize(command).some((token) => token === "gh" || token.endsWith("/gh"));
}

function hasGitInvocation(command: string): boolean {
	return tokenize(command).some((token) => token === "git" || token.endsWith("/git"));
}

function shellQuote(value: string): string {
	return `'${value.replace(/'/g, `'"'"'`)}'`;
}

function gitConfigValue(key: string): string | undefined {
	const result = spawnSync("git", ["config", "--get", key], { encoding: "utf8" });
	if (result.status !== 0) return undefined;
	const value = result.stdout.trim();
	return value || undefined;
}

function gitIdentityEnv(): Record<string, string> {
	const name = process.env.GIT_AUTHOR_NAME || process.env.GIT_COMMITTER_NAME || gitConfigValue("user.name");
	const email = process.env.GIT_AUTHOR_EMAIL || process.env.GIT_COMMITTER_EMAIL || gitConfigValue("user.email");
	const env: Record<string, string> = {};
	if (name) {
		env.GIT_AUTHOR_NAME = name;
		env.GIT_COMMITTER_NAME = process.env.GIT_COMMITTER_NAME || name;
	}
	if (email) {
		env.GIT_AUTHOR_EMAIL = email;
		env.GIT_COMMITTER_EMAIL = process.env.GIT_COMMITTER_EMAIL || email;
	}
	return env;
}

function isInside(child: string, parent: string): boolean {
	const relative = path.relative(parent, child);
	return relative === "" || (!!relative && !relative.startsWith("..") && !path.isAbsolute(relative));
}

function findRepoRoot(start = process.cwd()): string | undefined {
	let current = path.resolve(start);
	while (true) {
		if (fs.existsSync(path.join(current, ".git"))) return current;
		const parent = path.dirname(current);
		if (parent === current) return undefined;
		current = parent;
	}
}

function resolvedTargetPath(inputPath: string): string {
	const absolute = path.resolve(process.cwd(), inputPath);
	if (fs.existsSync(absolute)) return fs.realpathSync.native(absolute);
	const parent = fs.realpathSync.native(path.dirname(absolute));
	return path.join(parent, path.basename(absolute));
}

function modeSandbox(command: string, mode: GhMode): string {
	if (mode === "publish") return command;
	const repoRoot = findRepoRoot();
	if (!repoRoot) return command;

	const setup = ["export GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0"];
	if (mode === "local") {
		setup.push("export GIT_ASKPASS=false GIT_SSH_COMMAND='sh -c \"exit 1\"' GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=");
		for (const [key, value] of Object.entries(gitIdentityEnv())) {
			setup.push(`export ${key}=${shellQuote(value)}`);
		}
	}
	setup.push(command);
	const wrapped = setup.join("; ");

	const helper = process.env.PI_SQUARE_GH_HELPER;
	if (!helper) {
		return "printf '%s\\n' 'GitHub mode sandbox helper is not configured.' >&2; exit 127";
	}
	return [helper, "--pi-square-internal-gh-sandbox", mode, process.cwd(), wrapped].map(shellQuote).join(" ");
}

export default function ghModeExtension(pi: ExtensionAPI): void {
	// The source lives in this repository, but the extension is enabled only by
	// the pi-square wrapper, which embeds it and sets this marker.
	if (process.env.PI_SQUARE_ACTIVE !== "1") return;

	let mode: GhMode = "browse";
	const githubCalls = new Set<string>();

	function persistMode(): void {
		pi.appendEntry(STATE_ENTRY, { mode });
	}

	function setMode(next: GhMode, ctx: ExtensionContext, notify = true): void {
		mode = next;
		updateStatus(ctx);
		persistMode();
		if (notify) ctx.ui.notify(`${GITHUB_ICON} GitHub mode: ${mode}`, "info");
	}

	function updateStatus(ctx: ExtensionContext): void {
		const color = mode === "browse" ? "success" : mode === "local" ? "accent" : "warning";
		ctx.ui.setStatus(STATUS_KEY, ctx.ui.theme.fg(color, mode));
	}

	async function patchGhToken(event: { input: { command?: string }; toolName: string; toolCallId: string }) {
		if (event.toolName !== "bash" || typeof event.input.command !== "string") return;
		const command = event.input.command;
		const usesGh = hasGhInvocation(command);
		const usesGit = hasGitInvocation(command);
		const usesGithub = usesGh || usesGit;
		if (usesGithub) githubCalls.add(event.toolCallId);

		// In browse/local modes, hide the write token from every shell so raw HTTP
		// clients (curl, wget, etc.) cannot bypass gh-mode enforcement with
		// $PI_GH_W_TOKEN. gh always uses the read-only token outside publish mode.
		if (mode !== "publish") {
			if (usesGithub) {
				const tokenEnv = usesGh ? explicitGhTokenEnv(command) || tokenEnvName(mode) : tokenEnvName(mode);
				if (tokenEnv === W_TOKEN_ENV) {
					return { block: true, reason: `GitHub mode is ${mode}: ${W_TOKEN_ENV} is only available in publish mode.` };
				}
				if (usesGh && !process.env[tokenEnv]) {
					return { block: true, reason: `GitHub ${mode} token is not configured. Set ${tokenEnv}.` };
				}
				event.input.command = modeSandbox(usesGit ? prefixGitToken(command, mode) : `unset ${W_TOKEN_ENV}; ${prefixGhToken(command, mode)}`, mode);
				return;
			}
			event.input.command = modeSandbox(`unset ${W_TOKEN_ENV}; ${command}`, mode);
			return;
		}

		if (!usesGithub) return;

		const tokenEnv = usesGh ? explicitGhTokenEnv(command) || tokenEnvName(mode) : tokenEnvName(mode);
		if (!process.env[tokenEnv]) {
			return { block: true, reason: `GitHub ${mode} token is not configured. Set ${tokenEnv}.` };
		}

		event.input.command = usesGit ? prefixGitToken(command, mode) : prefixGhToken(command, mode);
	}

	async function guardGitDirEdits(event: { input: { path?: string }; toolName: string }) {
		if (mode !== "browse" || !["edit", "write"].includes(event.toolName) || typeof event.input.path !== "string") return;
		const repoRoot = findRepoRoot();
		if (!repoRoot) return;
		const gitDir = fs.realpathSync.native(path.join(repoRoot, ".git"));
		let target: string;
		try {
			target = resolvedTargetPath(event.input.path);
		} catch {
			return { block: true, reason: `GitHub mode is browse: cannot resolve ${event.input.path} for .git protection.` };
		}
		if (isInside(target, gitDir)) {
			return { block: true, reason: "GitHub mode is browse: edits to .git are not allowed. Switch to /gh-mode local for local git repository changes." };
		}
	}

	function explainFailedGithubCommand(event: ToolResultEvent) {
		const wasGithubCommand = githubCalls.delete(event.toolCallId);
		if (mode === "publish" || !event.isError || event.toolName !== "bash" || !wasGithubCommand) return;

		const hint = mode === "browse"
			? "Switch to /gh-mode local for local repository changes, or /gh-mode publish for remote writes."
			: "Switch to /gh-mode publish if the command needs remote write access.";
		return { content: [...event.content, { type: "text" as const, text: `${GITHUB_ICON} GitHub mode is ${mode}. ${hint}` }] };
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
				ctx.ui.notify(`${GITHUB_ICON} GitHub mode: ${mode}\n${RO_TOKEN_ENV}: ${process.env[RO_TOKEN_ENV] ? "set" : "missing"}\n${W_TOKEN_ENV}: ${process.env[W_TOKEN_ENV] ? "set" : "missing"}`, "info");
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
		mode = restoreMode(ctx, "browse");
		updateStatus(ctx);
	});

	pi.on("tool_call", guardGitDirEdits);
	pi.on("tool_call", patchGhToken);
	pi.on("tool_result", explainFailedGithubCommand);
}
