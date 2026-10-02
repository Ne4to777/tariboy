import { spawn, spawnSync } from "node:child_process";
import { chmodSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const uiDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repoDir = path.resolve(uiDir, "..");
const testRoot = mkdtempSync(path.join(tmpdir(), "tariboy-tasks-e2e-"));
const baseDir = path.join(testRoot, "base");
const runtimeDir = path.join(testRoot, "runtime");
const daemonBin = path.join(testRoot, process.platform === "win32" ? "tariboyd.exe" : "tariboyd");
mkdirSync(baseDir);
mkdirSync(runtimeDir);

const build = spawnSync("go", ["build", "-trimpath", "-o", daemonBin, "./cmd/tariboyd"], {
  cwd: repoDir,
  stdio: "inherit",
});
if (build.status !== 0) {
  rmSync(testRoot, { recursive: true, force: true });
  throw new Error(`failed to build Tasks E2E daemon from the working tree (exit ${build.status})`);
}

const baseURL = "http://127.0.0.1:4176";
let closing = false;
let forceTimer;
let resolveDaemonExit;
const daemonExit = new Promise((resolve) => {
  resolveDaemonExit = resolve;
});
const daemonEnv = {
  HOME: testRoot,
  LANG: "C.UTF-8",
  LOGNAME: "tasks-e2e",
  PATH: "/usr/local/bin:/usr/bin:/bin",
  SHELL: "/bin/sh",
  TMPDIR: testRoot,
  USER: "tasks-e2e",
  TARIBOY_BASE_DIR: baseDir,
  TARIBOY_RUNTIME_DIR: runtimeDir,
};

const daemon = spawn(daemonBin, ["--http-addr", "127.0.0.1:4176", "--log-level", "error"], {
  cwd: repoDir,
  env: daemonEnv,
  stdio: "inherit",
});

// A built workflow image is stored read-only; rmSync cannot delete it until the
// owner may write again.
function makeWritable(dir) {
  chmodSync(dir, 0o700);
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const entryPath = path.join(dir, entry.name);
    if (entry.isDirectory()) makeWritable(entryPath);
    else if (!lstatSync(entryPath).isSymbolicLink()) chmodSync(entryPath, 0o600);
  }
}

function removeState() {
  try {
    makeWritable(testRoot);
  } catch {
    // Nothing to unlock when the state is already gone.
  }
  rmSync(testRoot, { recursive: true, force: true });
}

function shutdown() {
  if (closing) return;
  closing = true;
  daemon.kill("SIGTERM");
  forceTimer = setTimeout(() => daemon.kill("SIGKILL"), 5_000);
}

process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
daemon.on("exit", (code, signal) => {
  if (forceTimer) clearTimeout(forceTimer);
  removeState();
  if (!closing) {
    process.exitCode = code ?? (signal ? 1 : 0);
  }
  resolveDaemonExit();
});

for (let attempt = 0; attempt < 100; attempt += 1) {
  if (daemon.exitCode !== null || daemon.signalCode !== null) {
    throw new Error("isolated Tasks E2E daemon exited before becoming ready");
  }
  let response;
  try {
    response = await fetch(`${baseURL}/api/daemon/status`);
  } catch {
    // The daemon is still starting.
  }
  if (response?.ok) {
    const envelope = await response.json();
    if (envelope?.result?.base_dir !== baseDir) {
      shutdown();
      throw new Error(`refusing daemon at ${baseURL}: it does not own the isolated test state`);
    }
    console.log(`isolated Tasks E2E daemon ready at ${baseURL}`);
    break;
  }
  if (attempt === 99) {
    shutdown();
    throw new Error(`isolated Tasks E2E daemon did not start at ${baseURL}`);
  }
  await new Promise((resolve) => setTimeout(resolve, 100));
}

await daemonExit;
