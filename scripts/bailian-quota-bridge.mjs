import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const EXPECTED_PKG = "bailian-cli";
const EXPECTED_VERSION = "2.7.2";
const ALLOWED_APIS = new Set([
  "zeldaHttp.dashscopeModel./zelda/api/v1/modelCenter/listFoundationModels",
  "zeldaEasy.bailian-commerce.freeTrial.queryFreeTierQuota",
  "zeldaEasy.bailian-commerce.freeTrial.queryFreeTierOnlyStatus",
]);

function redactSecrets(text, secrets) {
  let out = text;
  for (const s of secrets) {
    if (typeof s === "string" && s.length > 0) out = out.split(s).join("[REDACTED]");
  }
  return out;
}

async function readStdinUTF8() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString("utf8").trim();
}

async function main() {
  const pkgDir = process.argv[2];
  if (!pkgDir) throw new Error("Usage: bailian-quota-bridge.mjs <bailian_cli_pkg_dir>");

  const pkg = JSON.parse(fs.readFileSync(path.join(pkgDir, "package.json"), "utf8"));
  if (pkg?.name !== EXPECTED_PKG || pkg?.version !== EXPECTED_VERSION) {
    throw new Error(`Unsupported bailian-cli package at ${pkgDir}: expected ${EXPECTED_PKG}@${EXPECTED_VERSION}, got ${pkg?.name}@${pkg?.version}`);
  }

  let creds, cliAccessToken;
  try {
    const rawStdin = await readStdinUTF8();
    if (!rawStdin) throw new Error("Missing credentials JSON on stdin");
    creds = JSON.parse(rawStdin);
    if (!creds?.accessKeyId || !creds?.accessKeySecret) throw new Error("Missing accessKeyId or accessKeySecret");

    const coreUrl = pathToFileURL(path.join(pkgDir, "node_modules/bailian-cli-core/dist/index.mjs")).href;
    const cmdsUrl = pathToFileURL(path.join(pkgDir, "node_modules/bailian-cli-commands/dist/index.mjs")).href;
    const { generateCLIAccessToken, callConsoleGateway, resolveCLIAccessTokenTarget } = await import(coreUrl);
    const { usageFree } = await import(cmdsUrl);

    const identity = { binName: "bl", version: EXPECTED_VERSION, npmPackage: EXPECTED_PKG, clientName: "magpie-keeper" };
    const timeout = Number.isFinite(Number(creds.timeout)) && Number(creds.timeout) > 0 ? Number(creds.timeout) : 30;
    const baseUrl = creds.baseUrl || "https://dashscope.aliyuncs.com";
    const settings = {
      output: "json", outputExplicit: true, dryRun: false, quiet: false, verbose: false,
      timeout, watermark: false, telemetry: false,
      consoleRegion: creds.consoleRegion || undefined,
      consoleSite: creds.consoleSite || undefined,
      consoleSwitchAgent: creds.consoleSwitchAgent,
    };

    const tokenResp = await generateCLIAccessToken({
      identity, settings, baseUrl,
      accessKeyId: creds.accessKeyId,
      accessKeySecret: creds.accessKeySecret,
      securityToken: creds.securityToken || undefined,
    });
    cliAccessToken = tokenResp?.cliAccessToken;
    if (!cliAccessToken) throw new Error("generateCLIAccessToken returned an empty cliAccessToken");

    const target = resolveCLIAccessTokenTarget(settings, baseUrl);
    const guardedConsole = async (api, data) => {
      if (!ALLOWED_APIS.has(api)) throw new Error(`Blocked non-allowlisted console API: ${api}`);
      return callConsoleGateway({ ...target, token: cliAccessToken }, settings.timeout, { api, data });
    };

    await usageFree.run({
      identity, settings,
      flags: { all: true, sort: "expires" },
      client: { console: guardedConsole },
      localize: (text) => text?.["en-US"] ?? text?.["zh-CN"] ?? "",
    });
  } catch (err) {
    const rawMsg = err instanceof Error ? err.message : String(err);
    throw new Error(redactSecrets(rawMsg, [creds?.accessKeyId, creds?.accessKeySecret, creds?.securityToken, cliAccessToken]));
  }
}

main().catch((err) => {
  process.stderr.write(`${err instanceof Error ? err.message : String(err)}\n`);
  process.exitCode = 1;
});
