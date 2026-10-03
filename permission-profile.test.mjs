// Run with: node --test permission-profile.test.mjs
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { stripTypeScriptTypes } from "node:module";
import vm from "node:vm";
import test from "node:test";

const source = stripTypeScriptTypes(fs.readFileSync(new URL("./permission-profile.ts", import.meta.url), "utf8"))
	.replace(/^import .*;\n/gm, "")
	.replace("export default function permissionProfileExtension", "function permissionProfileExtension");

const builtins = { defaultProfile: "browse", profiles: {
 browse: { workdir: "ro", github: "ro", net: "on", bash: "off", prompt: "Browse guidance." },
 local: { workdir: "rw", github: "ro", net: "on", bash: "on", prompt: "Local guidance." },
 publish: { workdir: "rw", github: "rw", net: "on", bash: "on", prompt: "Publish guidance." },
}};
function setup(initialProfile, entries = [], config = builtins, accept = true) {
	const handlers = {};
	const commands = {};
	let active = ["read", "bash", "edit", "write", "custom"];
	let status;
	const state = [];
	const pi = {
		on: (name, handler) => (handlers[name] ??= []).push(handler),
		registerCommand: (name, command) => { commands[name] = command; },
		registerShortcut() {},
		appendEntry: (type, data) => state.push({ type, data }),
		getActiveTools: () => [...active],
		setActiveTools: (names) => { active = names; },
	};
	const ctx = {
  hasUI: true,
		sessionManager: { getEntries: () => entries },
		ui: { select: async (_title, choices) => typeof accept === "function" ? accept(choices) : accept ? choices[0] : "Keep existing", setStatus: (_key, value) => { status = value; }, theme: { fg: (_color, value) => value }, notify() {} },
	};
	const env = { PI_SQUARE_ACTIVE: "1", PI_SQUARE_GH_HELPER: "/launcher", PI_SQUARE_CONFIG: JSON.stringify({ order: Object.keys(config.profiles), ...config }) };
	if (initialProfile) env.PI_SQUARE_INITIAL_PROFILE = initialProfile;
	const sandbox = vm.createContext({
		fs, path, Buffer, process: { env, cwd: () => process.cwd() },
		Key: { altSuper: () => "shortcut" },
		createLocalBashOperations: () => ({ exec: (command) => command }),
	});
	vm.runInContext(source + "\nglobalThis.extension = permissionProfileExtension;", sandbox);
	sandbox.extension(pi);
	return {
		env,
		active: () => active,
		status: () => status,
		state: () => state,
		commands: () => Object.keys(commands),
		profile: (profile) => commands["pi-square-profile"].handler(profile, ctx),
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

test("profile transitions synchronize bash availability, status, prompt and guard", async () => {
	const app = setup();
	await app.emit("session_start");
	for (const profile of ["browse", "local", "publish", "browse", "local", "browse"]) {
		await app.profile(profile);
		const enabled = profile !== "browse";
		assert.equal(app.active().includes("bash"), enabled);
		assert.deepEqual(Array.from(app.active()).filter((name) => name !== "bash"), ["read", "edit", "write", "custom"]);
		assert.match(app.status(), new RegExp(` ${enabled ? "on" : "off"}`));
		const prompt = await app.emit("before_agent_start", { systemPrompt: "base" });
		assert.ok(prompt.systemPrompt.includes(builtins.profiles[profile].prompt));
		assert.match(prompt.systemPrompt, enabled ? /bash tool is available/ : /bash tool is unavailable/);
		const event = { toolName: "bash", input: { command: "ls" } };
		const result = await app.emit("tool_call", event);
		if (enabled) {
			assert.equal(result, undefined);
			assert.match(event.input.command, new RegExp(`--pi-square-stub '${profile}' `));
		} else {
			assert.equal(result.block, true);
			assert.equal(event.input.command, "ls");
		}
	}
});

test("initial and restored profiles set availability", async () => {
	for (const profile of ["browse", "local", "publish"]) {
		const initial = setup(profile);
		await initial.emit("session_start");
		assert.equal(initial.active().includes("bash"), profile !== "browse");
		const restored = setup(undefined, [{ type: "custom", customType: "pi-square-profile-state", data: { profile: profile } }]);
		await restored.emit("session_start");
		assert.equal(restored.active().includes("bash"), false);
	}
});

test("the namespaced command writes namespaced profile state", async () => {
 const app = setup();
 await app.emit("session_start");
 assert.deepEqual(app.commands(), ["pi-square-profile"]);
 await app.profile("local");
 assert.deepEqual(JSON.parse(JSON.stringify(app.state())), [{ type: "pi-square-profile-state", data: { profile: "local" } }]);
 const restored = setup(undefined, [{ type: "custom", customType: "pi-square-profile-state", data: { profile: "local" } }], { ...builtins, defaultProfile: "publish" });
 await restored.emit("session_start");
 assert.match(restored.status(), /^local/);
});

test("custom profiles control independent resources and switch immediately", async () => {
 const config = { defaultProfile: "offline", profiles: {
  offline: { workdir: "rw", github: "ro", net: "off", bash: "on", prompt: "Work without network access." },
  review: { workdir: "ro", github: "ro", net: "on", bash: "off", prompt: "Review without making changes." },
 }};
 const app = setup(undefined, [], config, false);
 await app.emit("session_start");
 assert.match(app.status(), /offline.* off.* on/);
 await app.profile("review");
 assert.match(app.status(), /^review.* on.* off/);
 const event = { toolName: "bash", input: { command: "echo ok" } };
 const result = await app.emit("tool_call", event);
 assert.equal(result.block, true);
 assert.equal(event.input.command, "echo ok");
 const prompt = await app.emit("before_agent_start", { systemPrompt: "base" });
 assert.match(prompt.systemPrompt, /Review without making changes\./);
 assert.match(prompt.systemPrompt, /mandatory proxy/);
});

test("read-only custom profiles block writes without confirmation", async () => {
 const config = { defaultProfile: "review", profiles: { review: { workdir: "ro", github: "rw", net: "off", bash: "on", prompt: "Review locally." } } };
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

test("reload keeps a launch profile narrower than the default", async () => {
	const config = { ...builtins, defaultProfile: "local" };
	const app = setup("browse", [], config);
	await app.emit("session_start");
	const entries = app.state().map(({ type, data }) => ({ type: "custom", customType: type, data }));
	const reloaded = setup(undefined, entries, config);
	await reloaded.emit("session_start");
	assert.match(reloaded.status(), /^browse/);
	assert.equal(reloaded.active().includes("bash"), false);
});

test("toggle follows the configured order", async () => {
	const config = { ...builtins, order: ["browse", "publish", "local"] };
	const app = setup(undefined, [], config);
	await app.emit("session_start");
	await app.profile("toggle");
	assert.match(app.status(), /^publish/);
	await app.profile("toggle");
	assert.match(app.status(), /^local/);
});

test("a profile switch during the workdir prompt honors the new profile", async () => {
	let app;
	app = setup(undefined, [], builtins, async () => { await app.profile("local"); return "Keep existing"; });
	await app.emit("session_start");
	const allowed = await app.emit("tool_call", { toolName: "write", input: { path: "new-file" } });
	assert.equal(allowed, undefined);
	const config = { ...builtins, profiles: { ...builtins.profiles, review: { ...builtins.profiles.browse, prompt: "Review." } } };
	app = setup(undefined, [], config, async (choices) => { await app.profile("review"); return choices[0]; });
	await app.emit("session_start");
	const blocked = await app.emit("tool_call", { toolName: "write", input: { path: "new-file" } });
	assert.equal(blocked.block, true);
});

test("malformed bash calls and a missing helper fail cleanly", async () => {
	const app = setup("local");
	await app.emit("session_start");
	const malformed = await app.emit("tool_call", { toolName: "bash", input: {} });
	assert.equal(malformed.block, true);
	delete app.env.PI_SQUARE_GH_HELPER;
	const event = { toolName: "bash", input: { command: "ls" } };
	assert.equal(await app.emit("tool_call", event), undefined);
	assert.match(event.input.command, /exit 127$/);
});
