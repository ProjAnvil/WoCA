#!/usr/bin/env python3
"""sandbox-demo — the EE sandbox plane's acceptance driver (spec §12).

Runs inside the demo profile container (plain HTTP to the stack's
internal surfaces, the shared workspaces volume mounted read-only) and
asserts the four acceptance levels end to end:

  L1 scripted model   empty workspace -> run executes inside the sandbox
                      (product + python test + egress probe land on the
                      volume) -> the sandbox container is reclaimed.
  L2 cross-run        a second run on the same workspace reads round
                      one's file, in a FRESH container, while the
                      per-workspace run mutex keeps at most one live
                      sandbox (never parallel writers).
  L3 hot options      a backend options change (image swap) mid-run:
                      the running sandbox keeps its image, the next run's
                      sandbox carries the marker file.
  L4 permissions      another tenant gets its own workspace (isolation by
                      identity), host-path access fails closed (403), the
                      egress probe inside the sandbox failed (net:blocked)
                      and no sandbox containers are left behind.

Exit code 0 = all levels green; any failure exits 1 with the assertion
in the log (the make target propagates the code).
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

SCHED = os.environ.get("WOCA_DEMO_SCHEDULER", "http://woca-scheduler-1:8080")
GW = os.environ.get("WOCA_DEMO_GW", "http://woca-scheduler-1:8090")
CONFIG = os.environ.get("WOCA_DEMO_CONFIG", "http://woca-config:8080")
PROXY = os.environ.get("WOCA_DEMO_PROXY", "http://executor-proxy:8094")
EXECUTOR = os.environ.get("WOCA_DEMO_EXECUTOR", "http://woca-executor:8093")
SB_KEY = os.environ.get("WOCA_DEMO_SB_API_KEY", "woca-demo-sb-key")
MARKER_IMAGE = os.environ.get("WOCA_DEMO_MARKER_IMAGE", "woca-sandbox-marker:demo")
MARKER_CONTENT = "woca-demo-marker-ok"
WORKSPACES = os.environ.get("WOCA_DEMO_WORKSPACES", "/data/workspaces")
WORKSPACE = "/data/demo-ws"  # the path tenants address; binds by tenant+path

POLL = 3


class Fail(Exception):
    pass


def check(cond, msg):
    if not cond:
        raise Fail(msg)


# ───────────────────────── HTTP helpers ─────────────────────────


def call(method, url, key=None, body=None, timeout=30):
    """One HTTP call; returns (status, text). Non-2xx is data, not error."""
    req = urllib.request.Request(url, method=method)
    if key:
        req.add_header("Authorization", "Bearer " + key)
    data = None
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = json.dumps(body).encode()
    try:
        with urllib.request.urlopen(req, data=data, timeout=timeout) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def ok(method, url, key=None, body=None, timeout=30):
    status, text = call(method, url, key, body, timeout)
    check(status == 200, "%s %s -> %d: %s" % (method, url, status, text[:400]))
    return text


def wait_for(desc, timeout, probe):
    """Poll until probe() returns (True, info) or fail at the timeout."""
    deadline = time.time() + timeout
    while True:
        good, info = probe()
        if good:
            return info
        if time.time() > deadline:
            raise Fail("%s (last state: %s)" % (desc, info))
        time.sleep(POLL)


def mint_key(tag):
    """A tenant-admin key under a REAL tenant: keys minted without a
    tenant_id land on the platform tenant "local", which never binds
    sandbox workspaces (the personal-posture guard in bindingTarget)."""
    name = "demo-" + tag
    status, text = call("POST", GW + "/api/tenant/tenants", body={"name": name})
    tid = json.loads(text).get("id", "") if status == 200 else ""
    if not tid:  # stack reuse — the tenant already exists; look it up
        for row in json.loads(ok("GET", GW + "/api/tenant/tenants")):
            if row.get("name") == name:
                tid = row.get("id", "")
    check(tid != "", "tenant %r not resolvable: %s" % (name, text[:200]))
    text = ok("POST", GW + "/api/tenant/users",
              body={"remark": name, "role": "tenant-admin", "tenant_id": tid})
    key = json.loads(text).get("key", "")
    check(key.startswith("woca_"), "minted key malformed: %s" % text[:200])
    return key


def submit_run(key, session, prompt):
    status, text = call("POST", SCHED + "/api/runs", key, {
        "session_id": session, "prompt": prompt, "workspace": WORKSPACE})
    if status != 200:
        return None, "%d: %s" % (status, text[:200])
    doc = json.loads(text)
    return doc.get("run_id", ""), text[:200]


def wait_run_done(key, run_id, timeout=420):
    """Follow the run's SSE ledger replay until the terminal frame."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        req = urllib.request.Request(SCHED + "/api/runs/" + run_id + "/events")
        req.add_header("Authorization", "Bearer " + key)
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                buf = ""
                while time.time() < deadline:
                    chunk = resp.readline()
                    if not chunk:
                        break
                    line = chunk.decode(errors="replace")
                    buf += line
                    if line.startswith("event: error"):
                        raise Fail("run %s errored: %s" % (run_id, buf[-600:]))
                    if line.startswith("event: done"):
                        return
                # stream ended without a terminal frame — reconnect for the replay
        except urllib.error.HTTPError as e:
            raise Fail("run %s events: %d %s" % (run_id, e.code, e.read().decode()[:200]))
        time.sleep(POLL)
    raise Fail("run %s never finished (timeout)" % run_id)


def run_until_admitted(key, session, prompt, timeout=360):
    """Submit a run, retrying while the gate warms up (403 pack, 503 ack);
    returns the admitted run_id."""
    holder = {}

    def probe():
        run_id, info = submit_run(key, session, prompt)
        if run_id:
            holder["run_id"] = run_id
            return True, ""
        return False, info

    wait_for("run %r never admitted" % prompt.split()[0], timeout, probe)
    return holder["run_id"]


def sandboxes():
    """The executor's live-sandbox listing (auth: the backend API key)."""
    text = ok("GET", EXECUTOR + "/internal/sandboxes", SB_KEY)
    return json.loads(text).get("sandboxes", [])


def live_sandboxes():
    return [s for s in sandboxes() if s.get("running")]


# ───────────────────────── volume helpers ─────────────────────────


def workspace_dirs():
    """Workspace directories on the shared volume (staging siblings excluded)."""
    try:
        names = os.listdir(WORKSPACES)
    except FileNotFoundError:
        return []
    return [os.path.join(WORKSPACES, n) for n in names
            if ".staging" not in n and os.path.isdir(os.path.join(WORKSPACES, n))]


def read_product(name):
    """Find <name> under exactly one workspace dir; returns (dir, content)."""
    hits = [(d, open(os.path.join(d, name)).read())
            for d in workspace_dirs() if os.path.isfile(os.path.join(d, name))]
    check(len(hits) == 1, "product %s found in %d workspace dirs (want 1): %s"
          % (name, len(hits), [d for d, _ in hits]))
    return hits[0]


# ───────────────────────── levels ─────────────────────────


def level1(key_a):
    print("== L1: scripted model run (empty workspace -> product -> reclaimed)")
    run_id = run_until_admitted(key_a, "demo-s1", "woca-demo:build round one")

    # While the run is in flight exactly its sandbox container is live on
    # the executor — execution really is off-host (no local fallback).
    wait_for("no live sandbox while run %s executes" % run_id, 120,
             lambda: (len(live_sandboxes()) >= 1,
                      "sandboxes=%s" % json.dumps(sandboxes())))
    wait_run_done(key_a, run_id)

    _, out = read_product("out.txt")
    check(out.strip() == "woca-demo-r1", "out.txt = %r" % out)
    _, test = read_product("test_ok.txt")
    check(test.strip() == "pass", "test_ok.txt = %r (sandbox-side unittest failed)" % test)
    _, net = read_product("net.txt")
    check(net.strip() == "net:blocked", "net.txt = %r (egress must fail closed)" % net)
    host_dir, host = read_product("host.txt")
    check(host.strip() != "", "host.txt empty")

    wait_for("sandbox containers not reclaimed after run %s" % run_id, 60,
             lambda: (len(sandboxes()) == 0, "sandboxes=%s" % json.dumps(sandboxes())))
    print("   product synced to the volume; unittest pass; egress blocked; sandbox reclaimed")
    return host_dir, host.strip()


def level2(key_a, host_r1):
    print("== L2: cross-run continuity, fresh container, per-workspace mutex")
    # Two runs on the same workspace submitted back to back: the claim
    # mutex serializes them (asserted live: never more than one sandbox).
    run_a = run_until_admitted(key_a, "demo-s2", "woca-demo:build round two")
    run_b = run_until_admitted(key_a, "demo-s3", "woca-demo:continue round two")

    def single_live():
        live = live_sandboxes()
        return len(live) <= 1, "live sandboxes: %s" % json.dumps(live)

    deadline = time.time() + 420
    while True:
        good, info = single_live()
        check(good, "workspace mutex violated (parallel sandboxes): " + info)
        done = len(sandboxes()) == 0  # both runs finished and reclaimed
        if done or time.time() > deadline:
            break
        time.sleep(1)
    wait_run_done(key_a, run_a)
    wait_run_done(key_a, run_b)

    _, round2 = read_product("round2.txt")
    check(round2.strip() == "woca-demo-r1",
          "round2.txt = %r (round two must read round one's file)" % round2)
    _, host2 = read_product("host2.txt")
    check(host2.strip() != host_r1,
          "host2.txt = %r equals round one's %r (sandbox must be a fresh container)"
          % (host2, host_r1))
    print("   round two read round one's file in a fresh container; runs serialized")


def level3(key_a):
    print("== L3: backend options hot-reload (image swap, no restarts)")
    backends = json.loads(ok("GET", CONFIG + "/api/sandboxes"))
    enabled = [b for b in backends if b.get("enabled")]
    check(len(enabled) == 1, "expected exactly one enabled backend, got %s" % json.dumps(backends))
    backend = enabled[0]
    opts = backend.get("options") or {}
    marker_opts = {
        "image": MARKER_IMAGE,
        "runtime": opts.get("runtime", "runc"),
        "network": opts.get("network", "none"),
        "cpu": opts.get("cpu", 0),
        "memory_mb": opts.get("memory_mb", 0),
    }

    # A long run whose sandbox is created BEFORE the change; mid-flight the
    # console swaps the image. The running sandbox keeps the base image.
    run_long = run_until_admitted(key_a, "demo-s4", "woca-demo:long run")
    wait_for("long run %s never started executing" % run_long, 120,
             lambda: (len(live_sandboxes()) >= 1, "sandboxes=%s" % json.dumps(sandboxes())))
    ok("PUT", CONFIG + "/api/sandboxes/" + backend["id"], body={"options": marker_opts})
    wait_run_done(key_a, run_long)
    _, img = read_product("img.txt")
    check(img.strip() == "base",
          "img.txt = %r (a running sandbox must keep its original image)" % img)

    # The next run's sandbox is created from the new options: the marker
    # file only exists on the marker image. Retry while the pack applies.
    def marker_landed():
        run_m = run_until_admitted(key_a, "demo-s5", "woca-demo:marker run")
        wait_run_done(key_a, run_m)
        try:
            _, marker = read_product("marker.txt")
        except Fail as e:
            return False, str(e)
        if marker.strip() != MARKER_CONTENT:
            return False, "marker.txt = %r" % marker
        return True, ""

    wait_for("hot options never took effect (marker image not used by new sandboxes)",
             300, marker_landed)
    print("   running sandbox unaffected; next sandbox used %s" % MARKER_IMAGE)


def level4(key_a, key_b):
    print("== L4: permissions and cleanup")
    # Another tenant addressing the same workspace path gets its OWN
    # workspace identity (isolation by tenant|path, never shared files):
    # tenant B runs the same build script against the same path, and its
    # product lands in a second workspace directory — the session surface
    # itself stays operator-scoped (S-B-0003), runs are the tenant plane.
    run_b = run_until_admitted(key_b, "demo-s6", "woca-demo:build tenant b")
    wait_run_done(key_b, run_b)
    with_out = [d for d in workspace_dirs() if os.path.isfile(os.path.join(d, "out.txt"))]
    check(len(with_out) == 2,
          "tenant B's product did not land in its own workspace (dirs with out.txt: %s)" % with_out)
    host_b = open(os.path.join(with_out[1], "host.txt")).read().strip()
    host_a = open(os.path.join(with_out[0], "host.txt")).read().strip()
    check(host_a != host_b, "tenant B executed in tenant A's workspace container")

    # Host-path access fails closed: no session (or an unknown session)
    # never resolves to a host directory (403 both).
    status, _ = call("GET", SCHED + "/api/workspace", key_a)
    check(status == 403, "workspace without a session = %d (want 403)" % status)
    status, _ = call("GET", SCHED + "/api/workspace?session=bogus", key_a)
    check(status == 403, "workspace with unknown session = %d (want 403)" % status)

    # No leftover sandbox containers on the executor (the last destroy
    # trails the done frame — poll until the teardown lands).
    wait_for("sandbox containers not reclaimed at the end", 60,
             lambda: (len(sandboxes()) == 0, "sandboxes=%s" % json.dumps(sandboxes())))
    print("   tenant isolation, host-path 403s, zero leftover containers")


def main():
    print("sandbox-demo: waiting for the scheduler to come healthy…")
    wait_for("scheduler never became healthy", 300,
             lambda: (call("GET", SCHED + "/healthz")[0] == 200, "no healthz"))
    key_a = mint_key("a")
    key_b = mint_key("b")
    _, host_r1 = level1(key_a)
    level2(key_a, host_r1)
    level3(key_a)
    level4(key_a, key_b)
    print("sandbox-demo: ALL LEVELS GREEN")


if __name__ == "__main__":
    try:
        main()
    except Fail as e:
        print("sandbox-demo FAILED: %s" % e, file=sys.stderr)
        sys.exit(1)
