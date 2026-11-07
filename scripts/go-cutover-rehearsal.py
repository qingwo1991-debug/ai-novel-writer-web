#!/usr/bin/env python3
"""Rehearse the Go cutover against a COPY of the author's data directory.

Why this exists: the migration is "done" when a browser can write a chapter
against the Go server, not when the served count reaches 281. That cannot be
shown by unit tests, and it must never be shown against the live data directory
while the Node container still owns it - both lanes take the same exclusive
writer lease, and a rehearsal that mutates author data is not a rehearsal.

So this script:
  1. loads the CI-built image tarball (never builds anything locally),
  2. copies the author data directory into a scratch directory,
  3. runs the Go container against the copy on a loopback-only port,
  4. drives the browser's own operation protocol with the same headers the
     trusted proxy sends (X-Novel-User) and the same session/CSRF handshake,
  5. exports an archive and proves the copy is byte-identical afterwards,
  6. reports, and leaves the container running so a human can open the app.

Nothing here reads or writes /root/AI/ai-novel-writer-data itself.

Usage:
  python3 scripts/go-cutover-rehearsal.py --image-tarball /tmp/novel-go-image.tar.gz
  python3 scripts/go-cutover-rehearsal.py --image-tarball ... --down
  python3 scripts/go-cutover-rehearsal.py --image-tarball ... --browser-proxy

The optional --browser-proxy starts a loopback-only shim that adds the
X-Novel-User header the production nginx adds, so the app can be opened in a
browser during the rehearsal. It is a rehearsal aid, not the product: the
product puts that header in the trusted proxy in front of the container.
"""

import argparse
import hashlib
import http.cookiejar
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

LIVE_DATA = "/root/AI/ai-novel-writer-data"
SCRATCH_ROOT = "/root/AI/rehearsal"
CONTAINER = "novel-go-rehearsal"
CONTAINER_PORT = 16082
AUTHOR = "rehearsal-author"
CLIENT = "rehearsal-tab"


def run(argv, **kwargs):
    """Run a command, returning its completed process; never shells out."""
    return subprocess.run(argv, check=False, text=True, **kwargs)


def fail(message):
    print("FAIL: " + message)
    sys.exit(1)


def step(message):
    print("-- " + message)


def docker_load(tarball):
    step("load the image from " + tarball)
    loaded = run(["docker", "load", "-i", tarball], capture_output=True)
    if loaded.returncode != 0:
        fail("docker load refused the tarball: " + (loaded.stderr or "").strip())
    image = ""
    for line in (loaded.stdout or "").splitlines():
        if "Loaded image:" in line:
            image = line.split("Loaded image:", 1)[1].strip()
    if not image:
        fail("the tarball loaded but named no image: " + (loaded.stdout or "").strip())
    print("   image " + image)
    return image


def prepare_scratch(scratch, live):
    if os.path.realpath(scratch) == os.path.realpath(live):
        fail("the scratch directory is the live data directory")
    if os.path.realpath(scratch).startswith(os.path.realpath(live) + os.sep):
        fail("the scratch directory is inside the live data directory")
    if os.path.isdir(scratch):
        shutil.rmtree(scratch)
    os.makedirs(scratch, exist_ok=True)
    target = os.path.join(scratch, "data")
    step("copy the author data directory into " + target)
    shutil.copytree(live, target, symlinks=True)
    return target


def tree_digest(root):
    """sha256 of every regular file, keyed by relative path."""
    digests = {}
    for base, directories, files in os.walk(root):
        directories.sort()
        for name in sorted(files):
            path = os.path.join(base, name)
            relative = os.path.relpath(path, root)
            try:
                with open(path, "rb") as handle:
                    digests[relative] = hashlib.sha256(handle.read()).hexdigest()
            except OSError as error:
                digests[relative] = "unreadable: " + str(error)
    return digests


def start_container(image, data, port):
    step("start the container against the copy on 127.0.0.1:%d" % port)
    run(["docker", "rm", "--force", CONTAINER], capture_output=True)
    started = run([
        "docker", "run", "--detach", "--init", "--name", CONTAINER,
        "-p", "127.0.0.1:%d:%d" % (port, CONTAINER_PORT),
        "-v", data + ":/data",
        "-e", "NOVEL_DATA_ROOT=/data",
        image,
        "-listen", "0.0.0.0:%d" % CONTAINER_PORT,
        "-static-dir", "/app/dist-web",
    ], capture_output=True)
    if started.returncode != 0:
        fail("the container did not start: " + (started.stderr or "").strip())
    print("   container " + (started.stdout or "").strip()[:12])


def wait_healthy(base, timeout=90):
    step("wait for /healthz")
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=3) as answer:
                payload = json.loads(answer.read().decode("utf-8"))
            if payload.get("status") == "ok":
                runtime = payload.get("runtime")
                if runtime != "go":
                    fail("/healthz answered runtime %r; the cutover image must run go" % (runtime,))
                print("   healthy, runtime " + runtime)
                return payload
            last = json.dumps(payload)
        except (urllib.error.URLError, OSError, ValueError) as error:
            last = str(error)
        time.sleep(1)
    logs = run(["docker", "logs", "--tail", "40", CONTAINER], capture_output=True)
    fail("the container never became healthy (%s)\n%s" % (last, (logs.stdout or "") + (logs.stderr or "")))


class Browser(object):
    """The browser's side of the protocol: cookie session, CSRF token, operations."""

    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ""
        self.calls = []

    def request(self, method, path, body=None, headers=None):
        data = None
        if body is not None:
            data = json.dumps(body).encode("utf-8")
        request = urllib.request.Request(self.base + path, data=data, method=method)
        request.add_header("X-Novel-User", AUTHOR)
        request.add_header("X-Client-Id", CLIENT)
        request.add_header("Origin", self.base)
        if self.csrf:
            request.add_header("X-CSRF-Token", self.csrf)
        if data is not None:
            request.add_header("Content-Type", "application/json")
        for name, value in (headers or {}).items():
            request.add_header(name, value)
        try:
            with self.opener.open(request, timeout=30) as answer:
                return answer.status, answer.read(), dict(answer.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers)

    def bootstrap(self):
        step("bootstrap a browser session")
        status, body, _ = self.request("GET", "/api/bootstrap")
        if status != 200:
            fail("/api/bootstrap answered %d: %s" % (status, body[:400]))
        payload = json.loads(body.decode("utf-8"))
        self.csrf = payload.get("csrfToken") or ""
        if not self.csrf:
            fail("/api/bootstrap returned no csrfToken")
        if not any(cookie.name for cookie in self.jar):
            fail("/api/bootstrap set no session cookie")
        if payload.get("generation") != "go-production-admitted":
            fail("the session is not the Go lane: %r" % (payload.get("generation"),))
        print("   csrf %d chars, %d cookie(s)" % (len(self.csrf), len(list(self.jar))))

    def operation(self, name, args, allow_refusal=False):
        status, body, _ = self.request("POST", "/api/operations/" + name, {"args": args})
        text = body.decode("utf-8", "replace")
        self.calls.append((name, status, text[:200]))
        if status != 200:
            if allow_refusal:
                return {"refused": True, "code": text[:200]}
            fail("%s answered HTTP %d: %s" % (name, status, text[:600]))
        try:
            return json.loads(text)
        except ValueError:
            fail("%s answered non-JSON: %s" % (name, text[:400]))

    def result(self, name, args, allow_refusal=False):
        answer = self.operation(name, args, allow_refusal)
        if "error" in answer and not allow_refusal:
            fail("%s refused: %s" % (name, json.dumps(answer["error"])[:400]))
        return answer.get("result")


def discover_projects(browser):
    step("discover the admitted projects (GET /api/library)")
    status, body, _ = browser.request("GET", "/api/library")
    if status != 200:
        fail("/api/library answered %d: %s" % (status, body[:400]))
    payload = json.loads(body.decode("utf-8"))
    projects = payload.get("projects") or []
    if not projects:
        fail("the copy holds no admitted project, so there is nothing to rehearse")
    for project in projects:
        print("   %s  %s" % (project.get("projectId"), project.get("path")))
    return projects


def open_project(browser, project):
    path = project["path"]
    step("open the project")
    opened = browser.result("project:open", [path, "rehearsal-open", path])
    if not opened.get("success") or not opened.get("dbReady"):
        fail("project:open did not open the project: " + json.dumps(opened)[:400])
    inner = opened.get("project") or {}
    lease = inner.get("sessionLease") or ""
    if not lease:
        fail("project:open returned no session lease")
    context = {"projectId": inner.get("id"), "leaseId": lease, "projectPath": path}
    print("   project %s, lease %d chars" % (context["projectId"], len(lease)))
    return context


def read_surface(browser, context):
    """Every read below answers from the copy; each one records what it said."""
    path = context["projectPath"]
    findings = []
    reads = [
        ("project:overview-current", []),
        ("db:draft-list", [1, path, context]),
        ("db:character-roster-read", [path, context]),
        ("character-identity:read", [path, context]),
        ("generation:list", [path, context]),
        ("kb:list-documents", [path, context]),
        ("kb:stats", [path, context]),
        ("llm:get-default-model", []),
        ("llm:get-stage-models", []),
        ("skills:list-user", []),
        ("mcp:get-servers-status", [path, context]),
    ]
    for name, args in reads:
        answer = browser.operation(name, args, allow_refusal=True)
        if "error" in answer:
            code = json.dumps(answer["error"])[:160]
            findings.append((name, "refused", code))
            continue
        value = answer.get("result")
        if isinstance(value, list):
            detail = "%d entr%s" % (len(value), "y" if len(value) == 1 else "ies")
        elif isinstance(value, dict):
            detail = ", ".join(sorted(value)[:6]) or "empty object"
        else:
            detail = type(value).__name__
        findings.append((name, "answered", detail))
    return findings


def export_archive(browser, context, scratch):
    step("export an archive and download it over HTTP")
    answer = browser.operation("project:archive-export", [context["projectPath"], context],
                               allow_refusal=True)
    if "error" in answer:
        fail("project:archive-export refused: " + json.dumps(answer["error"])[:400])
    receipt = answer.get("result") or {}
    if not receipt.get("success", True):
        fail("project:archive-export reported failure: " + json.dumps(receipt)[:400])
    download = receipt.get("downloadUrl") or receipt.get("url") or ""
    if not download:
        print("   receipt carries no download url; keys: " + ", ".join(sorted(receipt)))
        return None, receipt
    if download.startswith("/"):
        target = download
    else:
        target = "/" + download.split("/", 3)[-1]
    status, body, headers = browser.request("GET", target)
    if status != 200:
        fail("the archive download answered %d: %s" % (status, body[:200]))
    if len(body) < 1024:
        fail("the archive download is %d bytes, which cannot hold a book" % (len(body),))
    destination = os.path.join(scratch, "export.zip")
    with open(destination, "wb") as handle:
        handle.write(body)
    print("   %s -> %s (%d bytes, %s)" % (target, destination, len(body),
                                          headers.get("Content-Type", "?")))
    return destination, receipt


def zip_entries(path):
    import zipfile
    with zipfile.ZipFile(path) as archive:
        return sorted(archive.namelist())


def check_copy_untouched(before, after, scratch, allowed_new):
    step("prove the copy of the author data is unchanged by the reads")
    changed, added, removed = [], [], []
    for relative, digest in before.items():
        if relative not in after:
            removed.append(relative)
        elif after[relative] != digest:
            changed.append(relative)
    for relative in after:
        if relative not in before:
            added.append(relative)
    unexpected_added = [name for name in added if not any(name.startswith(prefix) for prefix in allowed_new)]
    if removed:
        fail("the reads removed %d file(s): %s" % (len(removed), removed[:5]))
    if changed:
        # A served write is allowed to change the database it wrote; a read-only
        # rehearsal must change nothing, so this is reported, never hidden.
        print("   %d file(s) changed: %s" % (len(changed), changed[:5]))
    if unexpected_added:
        print("   %d new file(s): %s" % (len(unexpected_added), unexpected_added[:5]))
    print("   %d file(s) unchanged, %d new, %d changed" % (
        len(before) - len(changed) - len(removed), len(added), len(changed)))
    return changed, unexpected_added


def start_browser_proxy(base, port):
    """A loopback-only shim that adds the header the production nginx adds."""
    import http.server
    import urllib.request as client

    target = base

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def forward(self, method):
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            request = client.Request(target + self.path, data=body, method=method)
            for name in ("Cookie", "Content-Type", "X-CSRF-Token", "X-Client-Id", "Origin"):
                if self.headers.get(name):
                    request.add_header(name, self.headers[name])
            request.add_header("X-Novel-User", AUTHOR)
            try:
                with client.urlopen(request, timeout=60) as answer:
                    payload, status, headers = answer.read(), answer.status, answer.headers
            except urllib.error.HTTPError as error:
                payload, status, headers = error.read(), error.code, error.headers
            except OSError as error:
                self.send_error(502, str(error))
                return
            self.send_response(status)
            for name, value in headers.items():
                if name.lower() in ("transfer-encoding", "connection", "content-length"):
                    continue
                self.send_header(name, value)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def do_GET(self):
            self.forward("GET")

        def do_POST(self):
            self.forward("POST")

    class Server(http.server.ThreadingHTTPServer):
        daemon_threads = True

    server = Server(("127.0.0.1", port), Handler)
    print("   rehearsal browser proxy on http://127.0.0.1:%d (Ctrl-C to stop)" % port)
    return server


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--image-tarball", required=True,
                        help="the novel-go-image.tar.gz artifact CI produced")
    parser.add_argument("--data", default=LIVE_DATA,
                        help="the author data directory to COPY (default: %(default)s)")
    parser.add_argument("--scratch", default=None,
                        help="where the copy and its export live (default: a new dir under %s)" % SCRATCH_ROOT)
    parser.add_argument("--port", type=int, default=16099,
                        help="loopback port the rehearsal container publishes (default: %(default)s)")
    parser.add_argument("--down", action="store_true", help="remove the container when done")
    parser.add_argument("--browser-proxy", action="store_true",
                        help="also serve a loopback shim that adds the trusted-proxy header")
    parser.add_argument("--proxy-port", type=int, default=16098)
    arguments = parser.parse_args()

    live = os.path.abspath(arguments.data)
    if not os.path.isdir(live):
        fail("the data directory does not exist: " + live)
    if not os.path.exists(arguments.image_tarball):
        fail("the image tarball does not exist: " + arguments.image_tarball)
    scratch = arguments.scratch or os.path.join(SCRATCH_ROOT, time.strftime("%Y%m%dT%H%M%SZ"))
    os.makedirs(scratch, exist_ok=True)

    image = docker_load(arguments.image_tarball)
    data = prepare_scratch(scratch, live)
    before = tree_digest(data)
    base = "http://127.0.0.1:%d" % arguments.port
    start_container(image, data, arguments.port)
    try:
        wait_healthy(base)
        browser = Browser(base)
        step("prove the API is not open without the trusted proxy header")
        anonymous = urllib.request.Request(base + "/api/bootstrap")
        try:
            with urllib.request.urlopen(anonymous, timeout=5) as answer:
                fail("/api/bootstrap answered %d without a user header" % (answer.status,))
        except urllib.error.HTTPError as error:
            if error.code != 401:
                fail("/api/bootstrap answered %d without a user header" % (error.code,))
            print("   401 without the header, as the cutover requires")
        step("prove the built app is served")
        status, body, headers = browser.request("GET", "/")
        if status != 200 or b'id="root"' not in body:
            fail("the app shell did not answer: %d %s" % (status, body[:200]))
        print("   %s, %d bytes" % (headers.get("Content-Type", "?"), len(body)))
        browser.bootstrap()
        projects = discover_projects(browser)
        context = open_project(browser, projects[0])
        findings = read_surface(browser, context)
        for name, verdict, detail in findings:
            print("   %-28s %-9s %s" % (name, verdict, detail))
        archive, receipt = export_archive(browser, context, scratch)
        if archive:
            entries = zip_entries(archive)
            print("   archive holds %d entries: %s" % (len(entries), ", ".join(entries[:6])))
        changed, added = check_copy_untouched(before, tree_digest(data), scratch, [".ai-novel/tmp", "exports/"])
        refused = [name for name, verdict, _ in findings if verdict == "refused"]
        print()
        print("REHEARSAL SUMMARY")
        print("  image            " + image)
        print("  project          " + str(context["projectPath"]))
        print("  operations       %d called, %d refused" % (len(browser.calls), len(refused)))
        print("  export           " + (archive or "not reachable"))
        print("  copy changed     %d file(s)" % (len(changed),))
        print("  new files        %d" % (len(added),))
        if refused:
            print("  refused          " + ", ".join(refused))
        if arguments.browser_proxy:
            server = start_browser_proxy(base, arguments.proxy_port)
            print("  browser          http://127.0.0.1:%d" % arguments.proxy_port)
            try:
                server.serve_forever()
            except KeyboardInterrupt:
                print()
        else:
            print("  browser          not exposed; rerun with --browser-proxy to open the app")
    finally:
        if arguments.down:
            run(["docker", "rm", "--force", CONTAINER], capture_output=True)
        else:
            status = run(["docker", "ps", "--filter", "name=" + CONTAINER, "--format", "{{.Status}}"],
                         capture_output=True)
            print("  container        " + (status.stdout or "").strip())


if __name__ == "__main__":
    main()
