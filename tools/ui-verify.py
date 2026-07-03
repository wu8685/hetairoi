#!/usr/bin/env python3
"""ui-verify: build a PR branch's ahsir, run an isolated scheduler+UI, seed a
test state, then drive the UI with Playwright (click-through + console-error
capture + screenshot). Prints PASS/FAIL. Reusable template for the dev loop.

Usage: ui_verify.py <repo> <branch> [out_png]
"""
import os, sys, json, shutil, subprocess, tempfile, time, glob, signal, pathlib

REPO   = sys.argv[1] if len(sys.argv) > 1 else "wu8685/ahsir"
BRANCH = sys.argv[2] if len(sys.argv) > 2 else "agent/issue-1"
OUT    = sys.argv[3] if len(sys.argv) > 3 else "/tmp/ui-verify.png"
TOKEN_FILE = os.path.expanduser("~/.cma-stack/github-token")
GO = "/usr/local/go/bin/go"
ENV = {**os.environ, "no_proxy": "127.0.0.1,localhost", "NO_PROXY": "127.0.0.1,localhost",
       "GO111MODULE": "on", "AHSIR_ADMIN_TOKEN": "ui-verify-token"}
SCHED_PORT, UI_PORT = 29900, 29901

work = tempfile.mkdtemp(prefix="ui-verify-")
procs = []
def sh(cmd, **kw): return subprocess.run(cmd, env=ENV, **kw)
def teardown():
    for p in procs:
        try: p.terminate()
        except Exception: pass
    time.sleep(1)
    subprocess.run(["pkill", "-f", f"registry http://127.0.0.1:{SCHED_PORT}"], env=ENV)
    shutil.rmtree(work, ignore_errors=True)

try:
    tok = pathlib.Path(TOKEN_FILE).read_text().strip()
    print(f"[1/5] clone {REPO}@{BRANCH}")
    sh(["git", "clone", "--depth", "1", "-b", BRANCH,
        f"https://x-access-token:{tok}@github.com/{REPO}.git", f"{work}/src"],
       check=True, capture_output=True)

    print("[2/5] build ahsir + ahsir-agent")
    sh([GO, "build", "-o", f"{work}/bin/ahsir", "./cmd/ahsir"], cwd=f"{work}/src", check=True)
    sh([GO, "build", "-o", f"{work}/bin/ahsir-agent", "./cmd/ahsir-agent"], cwd=f"{work}/src", check=True)

    # ---- seed test state: an offline/archived agent with a real transcript ----
    cfgdir = f"{work}/run"; agents = f"{cfgdir}/.ahsir/agents"
    os.makedirs(agents, exist_ok=True)
    src_ws = sorted(glob.glob(os.path.expanduser("~/.cma-stack/.ahsir/agents/cma-4u4d*")))[0]
    shutil.copytree(src_ws, f"{agents}/cma-issue-fixer-v1")
    with open(f"{cfgdir}/ahsir.yaml", "w") as f:
        f.write(f"registry: {{ host: \"127.0.0.1\", port: {SCHED_PORT}, heartbeat_interval: 10s, heartbeat_timeout: 30s }}\n")
        f.write("port_range: { start: 29911, end: 29920 }\n")

    print("[3/5] start scheduler + UI")
    procs.append(subprocess.Popen([f"{work}/bin/ahsir", "start", f"{cfgdir}/ahsir.yaml"],
                                  env=ENV, stdout=open(f"{work}/sched.log","w"), stderr=subprocess.STDOUT))
    for _ in range(40):
        if sh(["curl","-s","--noproxy","*",f"http://127.0.0.1:{SCHED_PORT}/agents"],capture_output=True).returncode==0 \
           and sh(["curl","-sf","--noproxy","*",f"http://127.0.0.1:{SCHED_PORT}/agents"],capture_output=True).returncode==0:
            break
        time.sleep(0.5)
    procs.append(subprocess.Popen([f"{work}/bin/ahsir","ui","--addr",f"127.0.0.1:{UI_PORT}",
                                   "--scheduler",f"http://127.0.0.1:{SCHED_PORT}"],
                                  env=ENV, stdout=open(f"{work}/ui.log","w"), stderr=subprocess.STDOUT))
    time.sleep(2)

    print("[4/5] drive the UI with Playwright (load -> click archived -> read transcript)")
    from playwright.sync_api import sync_playwright
    errors, result = [], {"archived_listed": False, "transcript_rendered": False}
    with sync_playwright() as p:
        b = p.chromium.launch(headless=True)
        pg = b.new_page(viewport={"width": 1500, "height": 1150})
        pg.on("console", lambda m: errors.append(m.text) if m.type == "error" else None)
        pg.on("pageerror", lambda e: errors.append(str(e)))
        pg.goto(f"http://127.0.0.1:{UI_PORT}/", wait_until="networkidle", timeout=20000)
        # archived section populated?
        arch = pg.locator("#archived .sess")
        arch.first.wait_for(timeout=10000)
        result["archived_listed"] = arch.count() > 0
        # click the archived context -> transcript should render in the main pane
        arch.first.click()
        pg.wait_for_load_state("networkidle", timeout=15000)
        time.sleep(1.5)
        # the issue-fixer transcript contains this text; assert it rendered
        body = pg.inner_text("body")
        result["transcript_rendered"] = ("cma-agent" in body) or ("empty repository" in body) or ("Pipeline smoke test" in body)
        pg.screenshot(path=OUT, full_page=False)
        b.close()

    print("[5/5] result")
    ok = result["archived_listed"] and result["transcript_rendered"] and not errors
    print(json.dumps({"pass": ok, **result, "console_errors": errors[:10]}, ensure_ascii=False, indent=1))
    print("screenshot:", OUT)
    sys.exit(0 if ok else 1)
finally:
    teardown()
