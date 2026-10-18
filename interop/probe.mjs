// Drives wakil-mcp with the official TypeScript SDK.
//
// The Go tests exercise the daemon with the Go SDK, which decodes into typed
// structs and is therefore forgiving: it cannot see a field that should not have
// been sent. This probe is a second, independent implementation of the protocol
// with a strict schema validator, and it is what caught a tools/list response
// that carried "outputSchema": null. The Go client accepted it; the TypeScript
// client rejected the entire list.
//
// Usage:
//
//   cd interop
//   npm install
//   node probe.mjs http://127.0.0.1:8765/mcp <client-token>
//
// The profile-restriction checks need a second token whose allow list is
// narrower than the first. Pass it in WAKIL_BOT_TOKEN; the checks that need it
// are skipped when it is absent.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const url = process.argv[2] ?? "http://127.0.0.1:8765/mcp";
const token = process.argv[3];
if (!token) {
  console.error("usage: node probe.mjs <url> <client-token>");
  process.exit(2);
}

const results = [];
function check(name, ok, detail) {
  results.push({ name, ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` — ${detail}` : ""}`);
}

async function connect(tok, label) {
  const transport = new StreamableHTTPClientTransport(new URL(url), {
    requestInit: { headers: { Authorization: `Bearer ${tok}` } },
  });
  const client = new Client({ name: `wakil-probe-${label}`, version: "1.0.0" });
  await client.connect(transport);
  return { client, transport };
}

function textOf(result) {
  return (result?.content ?? [])
    .filter((part) => part.type === "text")
    .map((part) => part.text)
    .join("\n");
}

const { client, transport } = await connect(token, "main");
const version =
  transport.protocolVersion ??
  (typeof client.getProtocolVersion === "function" ? client.getProtocolVersion() : undefined);
const serverInfo = client.getServerVersion();
const capabilities = client.getServerCapabilities();

console.log(`server:   ${serverInfo?.name} ${serverInfo?.version}`);
console.log(`protocol: ${version ?? "(not exposed by this SDK build)"}`);
console.log(`list changed notifications: ${capabilities?.tools?.listChanged === true}`);
console.log("");

// ---- tools/list ------------------------------------------------------------
const list = await client.listTools();
const names = list.tools.map((tool) => tool.name).sort();

check(
  "tools/list arrives and is not empty",
  names.length > 0,
  `${names.length} tools`,
);
check(
  "the exposed names are namespaced",
  names.filter((name) => !name.startsWith("_wakil.")).every((name) => name.includes(".")),
  names.filter((name) => !name.startsWith("_wakil.")).join(", "),
);
check(
  "the proxy's own tools are present",
  ["_wakil.describe_tool", "_wakil.list_servers", "_wakil.search_tools"].every((name) =>
    names.includes(name),
  ),
);

const echo = list.tools.find((tool) => tool.name.endsWith(".echo"));
check("descriptions survive the hop", typeof echo?.description === "string" && echo.description.length > 0);
check("input schemas survive the hop", echo?.inputSchema?.type === "object");
check(
  "every tool carries a schema that is an object, never null",
  list.tools.every((tool) => tool.inputSchema !== null && typeof tool.inputSchema === "object"),
  // This is the check that a permissive client cannot make for you.
  list.tools
    .filter((tool) => tool.inputSchema === null || typeof tool.inputSchema !== "object")
    .map((tool) => tool.name)
    .join(", ") || "all objects",
);

// ---- tools/call ------------------------------------------------------------
if (echo) {
  const result = await client.callTool({ name: echo.name, arguments: { text: "from the typescript sdk" } });
  check("tools/call reaches the upstream", textOf(result).includes("from the typescript sdk"), textOf(result).trim());
}

const errorTool = names.find((name) => name.endsWith(".error"));
if (errorTool) {
  const failure = await client.callTool({ name: errorTool, arguments: { text: "deliberate" } });
  check(
    "a tool-level error stays a tool result, not a protocol error",
    failure.isError === true,
    textOf(failure).trim().slice(0, 60),
  );
}

const described = await client.callTool({
  name: "_wakil.describe_tool",
  arguments: { name: names.find((name) => !name.startsWith("_wakil.")) },
});
check("the proxy's own tools answer", textOf(described).includes("inputSchema"));

let unknown = "";
try {
  await client.callTool({ name: "no-such-tool-anywhere", arguments: {} });
} catch (error) {
  unknown = String(error?.message ?? error);
}
check("an unknown tool is refused", unknown.includes("unknown tool"), unknown.slice(0, 70));

await client.close();

// ---- profiles --------------------------------------------------------------
const botToken = process.env.WAKIL_BOT_TOKEN;
if (botToken) {
  const bot = await connect(botToken, "restricted");
  const botNames = (await bot.client.listTools()).tools.map((tool) => tool.name).sort();
  const botUpstream = botNames.filter((name) => !name.startsWith("_wakil."));

  check(
    "a restricted profile sees only its own tools",
    botUpstream.length < names.length && botUpstream.length > 0,
    botUpstream.join(", "),
  );

  const listing = JSON.parse(
    textOf(await bot.client.callTool({ name: "_wakil.list_servers", arguments: {} })),
  );
  check(
    "the server listing is filtered to the profile",
    listing.serverCount > 0 && listing.servers.every((s) => botUpstream.some((n) => n.startsWith(`${s.name}.`))),
    JSON.stringify({ serverCount: listing.serverCount, toolCount: listing.toolCount }),
  );
  check(
    "the listed tool count describes the caller's roster",
    listing.toolCount === botUpstream.length,
    `toolCount=${listing.toolCount}, visible=${botUpstream.length}`,
  );

  let refused = "";
  const hidden = names.find((name) => !name.startsWith("_wakil.") && !botUpstream.includes(name));
  if (hidden) {
    try {
      await bot.client.callTool({ name: hidden, arguments: {} });
    } catch (error) {
      refused = String(error?.message ?? error);
    }
    check("a hidden tool is refused", refused.includes("not authorized"), refused.slice(0, 70));
  }

  await bot.client.close();
} else {
  console.log("SKIP  profile checks (set WAKIL_BOT_TOKEN to a narrower client token)");
}

const failed = results.filter((result) => !result.ok);
console.log("");
console.log(`${results.length - failed.length}/${results.length} checks passed`);

// Exiting immediately after the last request aborts Node on Windows with a
// libuv assertion ("!(handle->flags & UV_HANDLE_CLOSING)") — a teardown race in
// Node itself, which turns a passing run into a crash. Letting the handles
// settle first makes the exit code mean what it says.
await new Promise((resolve) => setTimeout(resolve, 600));
process.exit(failed.length === 0 ? 0 : 1);
