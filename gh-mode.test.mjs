// Run with: node --test gh-mode.test.mjs
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { stripTypeScriptTypes } from "node:module";
import vm from "node:vm";
import test from "node:test";

const source = stripTypeScriptTypes(fs.readFileSync(new URL("./gh-mode.ts", import.meta.url), "utf8"))
	.replace(/^import .*;\n/gm, "")
	.replace("export default function ghModeExtension", "function ghModeExtension");

const builtins = { defaultProfile: "browse", profiles: {
 browse: { workdir: "ro", github: "ro", net: "on", bash: "off" },
 local: { workdir: "rw", github: "ro", net: "on", bash: "on" },
 publish: { workdir: "rw", github: "rw", net: "on", bash: "on" },
}};
function setup(initialMode, entries = [], config = builtins, accept = true) {
	const handlers = {};
	const commands = {};
	let active = ["read", "bash", "edit", "write", "custom"];
	let status;
	const pi = {
		on: (name, handler) => (handlers[name] ??= []).push(handler),
		registerCommand: (name, command) => { commands[name] = command; },
		registerShortcut() {},
		appendEntry() {},
		getActiveTools: () => [...active],
		setActiveTools: (names) => { active = names; },
	};
	const ctx = {
  hasUI: true,
		sessionManager: { getEntries: () => entries },
		ui: { select: async (_title, choices) => accept ? choices[0] : "Keep existing", setStatus: (_key, value) => { status = value; }, theme: { fg: (_color, value) => value }, notify() {} },
	};
	const env = { PI_SQUARE_ACTIVE: "1", PI_SQUARE_GH_HELPER: "/launcher", PI_SQUARE_CONFIG: JSON.stringify(config) };
	if (initialMode) env.PI_SQUARE_INITIAL_GH_MODE = initialMode;
	const sandbox = vm.createContext({
		fs, path, Buffer, process: { env, cwd: () => process.cwd() },
		Key: { altSuper: () => "shortcut" },
		createLocalBashOperations: () => ({ exec: (command) => command }),
	});
	vm.runInContext(source + "\nglobalThis.extension = ghModeExtension;", sandbox);
	sandbox.extension(pi);
	return {
		active: () => active,
		status: () => status,
		mode: (mode) => commands["gh-mode"].handler(mode, ctx),
		emit: async (name, event = {}) => {
			let result;
			for (const handler of handlers[name] ?? []) {
				result = await handler(event, ctx) ?? result;
				if (result?.block) break;
			}
			return result;
		},
	};
}

test("mode transitions synchronize bash availability, status, prompt and guard", async () => {
	const app = setup();
	await app.emit("session_start");
	for (const mode of ["browse", "local", "publish", "browse", "local", "browse"]) {
		await app.mode(mode);
		const enabled = mode !== "browse";
		assert.equal(app.active().includes("bash"), enabled);
		assert.deepEqual(Array.from(app.active()).filter((name) => name !== "bash"), ["read", "edit", "write", "custom"]);
		assert.match(app.status(), new RegExp(` ${enabled ? "on" : "off"}`));
		const prompt = await app.emit("before_agent_start", { systemPrompt: "base" });
		assert.match(prompt.systemPrompt, enabled ? /bash tool is available/ : /bash tool is unavailable/);
		const event = { toolName: "bash", input: { command: "ls" } };
		const result = await app.emit("tool_call", event);
		if (enabled) {
			assert.equal(result, undefined);
			assert.match(event.input.command, new RegExp(`--pi-square-stub '${mode}' `));
		} else {
			assert.equal(result.block, true);
			assert.equal(event.input.command, "ls");
		}
	}
});

test("initial and restored modes set availability", async () => {
	for (const mode of ["browse", "local", "publish"]) {
		const initial = setup(mode);
		await initial.emit("session_start");
		assert.equal(initial.active().includes("bash"), mode !== "browse");
		const restored = setup(undefined, [{ type: "custom", customType: "gh-mode-state", data: { mode } }]);
		await restored.emit("session_start");
		assert.equal(restored.active().includes("bash"), false);
	}
});

test("custom profiles control independent resources and switch immediately", async () => {
 const config = { defaultProfile: "offline", profiles: {
  offline: { workdir: "rw", github: "ro", net: "off", bash: "on" },
  review: { workdir: "ro", github: "ro", net: "on", bash: "off" },
 }};
 const app = setup(undefined, [], config, false);
 await app.emit("session_start");
 assert.match(app.status(), /offline.* off.* on/);
 await app.mode("review");
 assert.match(app.status(), /^review.* on.* off/);
 const event = { toolName: "bash", input: { command: "echo ok" } };
 const result = await app.emit("tool_call", event);
 assert.equal(result.block, true);
 assert.equal(event.input.command, "echo ok");
 const prompt = await app.emit("before_agent_start", { systemPrompt: "base" });
 assert.match(prompt.systemPrompt, /mandatory proxy/);
});

test("read-only custom profiles block writes without confirmation", async () => {
 const config = { defaultProfile: "review", profiles: { review: { workdir: "ro", github: "rw", net: "off", bash: "on" } } };
 const app = setup(undefined, [], config, false);
 await app.emit("session_start");
 const result = await app.emit("tool_call", { toolName: "write", input: { path: "new-directory/new-file" } });
 assert.equal(result.block, true);
 assert.match(result.reason, /read-only/);
});

test("interactive shell commands still route through browse sandbox", async () => {
	const app = setup();
	await app.emit("session_start");
	const result = await app.emit("user_bash");
	assert.match(result.operations.exec("ls", "/tmp", {}), /--pi-square-stub 'browse' /);
});
