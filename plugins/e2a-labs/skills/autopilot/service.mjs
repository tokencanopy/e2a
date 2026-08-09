import { createHash } from "node:crypto";

function xml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");
}

function systemd(value) {
  return `"${String(value).replaceAll("\\", "\\\\").replaceAll('"', '\\"').replaceAll("%", "%%")}"`;
}

export function serviceIdentity(agentEmail) {
  const digest = createHash("sha256").update(agentEmail.toLowerCase(), "utf8").digest("hex").slice(0, 16);
  const slug = `agent-${digest}`;
  return {
    slug,
    launchdLabel: `dev.e2a.autopilot.${slug}`,
    systemdUnit: `e2a-autopilot-${slug}.service`,
  };
}

export function buildLaunchdDefinition(paths) {
  const environment = {
    E2A_AUTOPILOT_POLICY_PATH: paths.policyPath,
    E2A_AUTOPILOT_SECRETS_PATH: paths.secretsPath,
    E2A_AUTOPILOT_STATE_ROOT: paths.stateRoot,
    ...(paths.pathValue ? { PATH: paths.pathValue } : {}),
  };
  const environmentXml = Object.entries(environment)
    .map(([name, value]) => `      <key>${xml(name)}</key><string>${xml(value)}</string>`)
    .join("\n");
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<plist version="1.0">',
    "  <dict>",
    `    <key>Label</key><string>${xml(paths.label)}</string>`,
    "    <key>ProgramArguments</key>",
    "    <array>",
    `      <string>${xml(paths.nodePath)}</string>`,
    `      <string>${xml(paths.runnerPath)}</string>`,
    "    </array>",
    "    <key>EnvironmentVariables</key>",
    "    <dict>",
    environmentXml,
    "    </dict>",
    "    <key>RunAtLoad</key><true/>",
    "    <key>KeepAlive</key><true/>",
    `    <key>StandardOutPath</key><string>${xml(paths.stdoutPath)}</string>`,
    `    <key>StandardErrorPath</key><string>${xml(paths.stderrPath)}</string>`,
    "  </dict>",
    "</plist>",
    "",
  ].join("\n");
}

export function buildSystemdDefinition(paths) {
  return [
    "[Unit]",
    "Description=e2a Autopilot local supervisor",
    "After=network-online.target",
    "Wants=network-online.target",
    "",
    "[Service]",
    "Type=simple",
    `ExecStart=${systemd(paths.nodePath)} ${systemd(paths.runnerPath)}`,
    `Environment=${systemd(`E2A_AUTOPILOT_POLICY_PATH=${paths.policyPath}`)}`,
    `Environment=${systemd(`E2A_AUTOPILOT_SECRETS_PATH=${paths.secretsPath}`)}`,
    `Environment=${systemd(`E2A_AUTOPILOT_STATE_ROOT=${paths.stateRoot}`)}`,
    ...(paths.pathValue ? [`Environment=${systemd(`PATH=${paths.pathValue}`)}`] : []),
    `StandardOutput=append:${paths.stdoutPath}`,
    `StandardError=append:${paths.stderrPath}`,
    "Restart=on-failure",
    "RestartSec=5s",
    "",
    "[Install]",
    "WantedBy=default.target",
    "",
  ].join("\n");
}

export function serviceCommands({ manager, action, servicePath, identity, uid }) {
  if (manager === "foreground") return [];
  if (manager === "launchd") {
    const domain = `gui/${uid}`;
    switch (action) {
      case "start":
        return [["launchctl", ["bootstrap", domain, servicePath]]];
      case "stop":
        return [["launchctl", ["bootout", `${domain}/${identity.launchdLabel}`]]];
      case "status":
        return [["launchctl", ["print", `${domain}/${identity.launchdLabel}`]]];
      default:
        throw new Error(`Unsupported launchd action: ${action}.`);
    }
  }
  if (manager === "systemd") {
    switch (action) {
      case "start":
        return [
          ["systemctl", ["--user", "daemon-reload"]],
          ["systemctl", ["--user", "enable", "--now", identity.systemdUnit]],
        ];
      case "stop":
        return [["systemctl", ["--user", "disable", "--now", identity.systemdUnit]]];
      case "status":
        return [["systemctl", ["--user", "is-active", identity.systemdUnit]]];
      default:
        throw new Error(`Unsupported systemd action: ${action}.`);
    }
  }
  throw new Error(`Unsupported service manager: ${manager}.`);
}
