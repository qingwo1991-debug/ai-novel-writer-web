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
EXPORT_DIRECTORY = "rehearsal-export"
CLIENT = "rehearsal-tab-0001"  # 16-128 chars, internal/transport/server.go:78


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
    # The image runs as 65532:65532 (Dockerfile.go:39) and the writer lease refuses a
    # data root it does not own (internal/writerlock, safeDirectory). The production
    # compose instead runs the image as the data root's owner (user: 0:0), so either
    # would do; chowning the COPY is the one that matches how CI rehearses it
    # (.github/workflows/go-backend.yml:661).
    try:
        for base, directories, files in os.walk(target):
            os.chown(base, 65532, 65532)
            for name in files:
                os.chown(os.path.join(base, name), 65532, 65532, follow_symlinks=False)
    except PermissionError as error:
        fail("the copy could not be handed to uid 65532 (run as root): " + str(error))
    # The archive export writes into a directory the picker granted, and it refuses a
    # destination it cannot see. A desktop author picks an existing folder, so the
    # rehearsal creates one inside the library and hands the picker its name.
    library = os.path.join(target, "workspaces")
    if os.path.isdir(library):
        destination = os.path.join(library, EXPORT_DIRECTORY)
        os.makedirs(destination, exist_ok=True)
        os.chown(destination, 65532, 65532)
        os.chmod(destination, 0o700)
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
    # --network host, and the binary binds loopback inside: the migration build
    # refuses any other listen address by design (it is the scaffold's own guard,
    # .github/workflows/go-backend.yml:519-520), so port publishing cannot be used -
    # the journey step in CI runs it exactly this way too (:716-721). The container
    # therefore listens on the host's loopback only.
    started = run([
        "docker", "run", "--detach", "--init", "--name", CONTAINER,
        "--network", "host", "--read-only",
        "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
        "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777",
        "--mount", "type=bind,source=" + data + ",target=/data",
        "--env", "NOVEL_DATA_ROOT=/data",
        image,
        "-listen", "127.0.0.1:%d" % port,
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
            # The probe answers {ok, runtime, electron, migrationComplete}: `ok` is
            # the liveness member, and `runtime` is what distinguishes this image
            # from the Node one during a cutover window.
            if payload.get("ok") is True or payload.get("status") == "ok":
                runtime = payload.get("runtime")
                if runtime != "go":
                    fail("/healthz answered runtime %r; the cutover image must run go" % (runtime,))
                if payload.get("electron") is not False:
                    fail("/healthz still claims an electron runtime: " + json.dumps(payload))
                print("   healthy, runtime " + str(runtime) + ", migrationComplete " +
                      str(payload.get("migrationComplete")))
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
    """Mint a destination grant, export, and take the download the event names.

    The wire is the renderer's: a picker grants a directory inside the library,
    then project:archive-export takes {targetArchiveGrantId, projectSession} and
    publishes browser:download-ready on the live event stream. The stream has to be
    open FIRST - the hub delivers to the subscribers connected at that moment and
    replays nothing to a fresh stream (internal/transport/archive_export.go:113).
    """
    import threading
    step("mint a destination grant with the archive picker")
    picked = browser.operation("dialog:select-project-archive-export",
                               [{"rootId": "library", "relativePath": EXPORT_DIRECTORY}],
                               allow_refusal=True)
    if "error" in picked:
        fail("the archive picker refused: " + json.dumps(picked["error"])[:400])
    grant = (picked.get("result") or {}).get("grantId") or ""
    if not grant:
        fail("the archive picker returned no grantId: " + json.dumps(picked)[:400])
    print("   grant " + grant[:12] + "...")

    events = []
    diagnostics = []
    stop = threading.Event()

    def listen():
        request = urllib.request.Request(browser.base + "/api/events?client=" + CLIENT)
        request.add_header("X-Novel-User", AUTHOR)
        # The stream takes its tab from ?client=, not from the header
        # (internal/transport/server.go:102-105).
        request.add_header("X-Client-Id", CLIENT)
        request.add_header("Accept", "text/event-stream")
        request.add_header("Cookie", "; ".join("%s=%s" % (c.name, c.value) for c in browser.jar))
        try:
            with urlopen_with_timeout(request, 120) as stream:
                diagnostics.append("stream status %s, type %s" % (
                    stream.status, stream.headers.get("Content-Type", "?")))
                for raw in stream:
                    if stop.is_set():
                        return
                    line = raw.decode("utf-8", "replace").strip()
                    if line.startswith("data:"):
                        events.append(line[5:].strip())
        except urllib.error.HTTPError as error:
            diagnostics.append("stream answered HTTP %d: %s" % (error.code, error.read()[:200]))
        except (urllib.error.URLError, OSError) as error:
            diagnostics.append("stream failed: %s" % (error,))

    thread = threading.Thread(target=listen, daemon=True)
    thread.start()
    time.sleep(0.5)

    step("export the archive")
    answer = browser.operation("project:archive-export",
                               [{"targetArchiveGrantId": grant, "projectSession": context}],
                               allow_refusal=True)
    if "error" in answer:
        fail("project:archive-export refused: " + json.dumps(answer["error"])[:400])
    receipt = answer.get("result") or {}
    if not receipt.get("success", True):
        fail("project:archive-export reported failure: " + json.dumps(receipt)[:400])
    deadline = time.time() + 30
    url = ""
    while time.time() < deadline and not url:
        for payload in list(events):
            try:
                decoded = json.loads(payload)
            except ValueError:
                continue
            url = find_download_url(decoded, 0)
            if url:
                break
        if not url:
            time.sleep(0.2)
    stop.set()
    if not url:
        fail("the export finished but published no browser:download-ready url; "
             "events seen: " + json.dumps(events[:4])[:300] +
             "; " + "; ".join(diagnostics) +
             "; receipt keys: " + ", ".join(sorted(receipt)))
    status, body, headers = browser.request("GET", url)
    if status != 200:
        fail("the archive download answered %d: %s" % (status, body[:200]))
    if len(body) < 1024:
        fail("the archive download is %d bytes, which cannot hold a book" % (len(body),))
    destination = os.path.join(scratch, "export.zip")
    with open(destination, "wb") as handle:
        handle.write(body)
    print("   %s -> %s (%d bytes, %s)" % (url, destination, len(body),
                                          headers.get("Content-Type", "?")))
    return destination, receipt


def urlopen_with_timeout(request, seconds):
    return urllib.request.build_opener().open(request, timeout=seconds)


def find_download_url(value, depth):
    """The event carries {url, name} somewhere; find the download url in it."""
    if depth > 6:
        return ""
    if isinstance(value, dict):
        candidate = value.get("url")
        if isinstance(candidate, str) and candidate.startswith("/api/downloads/"):
            return candidate
        for item in value.values():
            found = find_download_url(item, depth + 1)
            if found:
                return found
    elif isinstance(value, list):
        for item in value:
            found = find_download_url(item, depth + 1)
            if found:
                return found
    return ""


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
