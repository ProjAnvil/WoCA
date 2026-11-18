import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { repoRoot, runtimeDir } from './paths';

// Dedicated-instance harness for personal specs that must OWN server-global
// state. `woca serve` keeps exactly ONE live provider slot (the settings
// `provider` key selects which of the four stored slots is active — no
// per-run override exists on POST /api/runs), one permission mode and one
// sidebar workspace index. The shared personal webServer (port 13710) hosts
// several spec files in parallel (fullyParallel + 4 workers), so two specs
// that reconfigure the provider race for that single slot: the loser's runs
// land on the winner's scripted model, or on a dead endpoint after the
// winner's teardown reset. Specs that mutate such state spawn their OWN
// `woca serve` (the binary the playwright webServer builds at e2e/.bin/woca,
// embedded web included) with an isolated WOCA_HOME: same real binary and
// web build under test, private provider slot, permission mode, sidebar and
// database — the same isolation the config's webServer applies.

/** A dedicated `woca serve` started by startDedicatedServe. */
export type DedicatedServe = {
  child: ChildProcess;
  /** The instance's origin, e.g. http://127.0.0.1:39887 */
  base: string;
};

/** A free loopback port (bind :0, read it, release — good enough locally). */
export function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      const port = addr && typeof addr === 'object' ? addr.port : 0;
      srv.close(() => (port ? resolve(port) : reject(new Error('no free port'))));
    });
    srv.on('error', reject);
  });
}

// Runtime hygiene (the posture the playwright webServer applies to
// personal-home): home-*/ws-* dirs from previous runs accrete under
// e2e/.runtime/<name>. Entry names embed their creating worker's pid
// (home-<pid>-<ts>), so "owner pid no longer alive" is exactly "left over
// from a previous run". Only those are wiped at startup: sibling workers of
// THIS run have live pids and unique names, so a blanket subtree wipe would
// race them (the config runs fullyParallel across 4 workers, each spawning
// its own serve home here).
export function wipeStaleRuntimeDirs(name: string): void {
  const dir = path.join(runtimeDir, name);
  fs.mkdirSync(dir, { recursive: true });
  for (const entry of fs.readdirSync(dir)) {
    const pid = Number(/^(?:home|ws)-(\d+)-\d+$/.exec(entry)?.[1]);
    if (!Number.isInteger(pid) || pid <= 0 || pid === process.pid) continue;
    try {
      process.kill(pid, 0); // owner still alive (or pid reused) — keep
      continue;
    } catch {
      // dead owner: leftover from a previous run
    }
    fs.rmSync(path.join(dir, entry), { recursive: true, force: true });
  }
}

/** Spawns the dedicated instance and waits for its /api/health to answer. */
export async function startDedicatedServe(name: string): Promise<DedicatedServe> {
  const bin = path.join(repoRoot, 'e2e', '.bin', 'woca');
  if (!fs.existsSync(bin)) {
    throw new Error(`${bin} missing — the playwright webServer builds it before tests run`);
  }
  wipeStaleRuntimeDirs(name);
  const home = path.join(runtimeDir, name, `home-${process.pid}-${Date.now()}`);
  fs.mkdirSync(home, { recursive: true });
  const port = await freePort();
  const child = spawn(
    bin,
    ['serve', '--addr', `127.0.0.1:${port}`, '--no-browser', '--log-level', 'warn'],
    {
      env: {
        ...process.env,
        HOME: home,
        WOCA_HOME: home,
        WOCA_DB: path.join(home, 'woca.db'),
        WOCA_PLUGIN_ROOT: path.join(home, 'plugins'),
        WOCA_NO_BROWSER: '1',
      },
      stdio: ['ignore', 'ignore', 'pipe'],
    },
  );
  const stderr: string[] = [];
  child.stderr?.on('data', (d: Buffer) => stderr.push(d.toString()));
  const at = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 30_000;
  for (;;) {
    if (child.exitCode !== null) {
      throw new Error(`dedicated woca serve exited early (code ${child.exitCode}): ${stderr.join('').slice(-500)}`);
    }
    try {
      const res = await fetch(`${at}/api/health`);
      if (res.ok) break;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) {
      child.kill('SIGKILL');
      throw new Error(`dedicated woca serve never became healthy: ${stderr.join('').slice(-500)}`);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  return { child, base: at };
}

/** Terminates the instance: SIGTERM first, escalating to SIGKILL after 5s. */
export async function stopDedicatedServe(serve: DedicatedServe): Promise<void> {
  const child = serve.child;
  child.kill('SIGTERM');
  await new Promise<void>((resolve) => {
    const t = setTimeout(() => {
      child.kill('SIGKILL');
      resolve();
    }, 5_000);
    child.once('exit', () => {
      clearTimeout(t);
      resolve();
    });
  });
}
