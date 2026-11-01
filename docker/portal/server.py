#!/usr/bin/env python3
"""LAN-only, unauthenticated Electron/noVNC portal on websockify's HTTP port."""

import errno
import json
import os
from pathlib import Path
import secrets
import stat
import subprocess
from urllib.parse import parse_qs, quote, urlencode, urlsplit

from websockify.websocketproxy import ProxyRequestHandler, WebSocketProxy

PORTAL_ROOT = Path("/opt/novel-portal")
AREAS = {"imports": "/data/imports", "exports": "/data/exports"}
MAX_UPLOAD = 64 * 1024 * 1024
MAX_CLIPBOARD = 1024 * 1024
MAX_LIST_ENTRIES = 2000
CHUNK = 64 * 1024
TEMP_PREFIX = ".portal-upload-"
CSP = (
    "default-src 'self'; script-src 'self'; style-src 'self'; "
    "frame-src 'self'; frame-ancestors 'none'; object-src 'none'; "
    "base-uri 'none'; form-action 'self'"
)


class PortalError(Exception):
    def __init__(self, status, message):
        self.status, self.message = status, message


def valid_name(name):
    if (not isinstance(name, str) or not name or name in (".", "..")
            or "/" in name or "\\" in name or name.startswith(TEMP_PREFIX)
            or any(ord(char) < 32 or ord(char) == 127 for char in name)):
        raise PortalError(400, "Invalid filename; use a plain basename")
    try:
        if len(name.encode("utf-8")) > 255:
            raise PortalError(400, "Filename is too long")
    except UnicodeEncodeError:
        raise PortalError(400, "Filename must be valid UTF-8") from None
    return name


def open_area(area):
    if area not in AREAS:
        raise PortalError(400, "Unknown file area")
    # Pin the directory for every operation; neither areas nor files may be symlinks.
    return os.open(AREAS[area], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)


def copy_clipboard(text, run=None):
    # xclip forks a clipboard-owning child. PIPE for stdout/stderr can leave
    # communicate() waiting for that child's inherited pipe indefinitely.
    (run or subprocess.run)(
        ["xclip", "-selection", "clipboard", "-target", "UTF8_STRING", "-in"],
        input=text.encode("utf-8"), stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL, timeout=5, check=True,
    )


class PortalHandler(ProxyRequestHandler):
    portal_root = PORTAL_ROOT

    def end_headers(self):
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "no-referrer")
        super().end_headers()

    def _reply(self, status, body, content_type="application/json; charset=utf-8",
               extra=()):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for key, value in extra:
            self.send_header(key, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _json(self, status, value):
        self._reply(status, json.dumps(value, ensure_ascii=True).encode("utf-8"))

    def _error(self, error):
        # Do not allow a rejected/unread POST body to become another request.
        self.close_connection = True
        self._json(error.status, {"error": error.message})

    def _route(self):
        try:
            return urlsplit(self.path)
        except ValueError:
            raise PortalError(400, "Malformed URL") from None

    def _params(self, query, expected):
        try:
            values = parse_qs(query, keep_blank_values=True, max_num_fields=8,
                              encoding="utf-8", errors="strict")
        except (ValueError, UnicodeError):
            raise PortalError(400, "Malformed query") from None
        if set(values) != set(expected) or any(len(v) != 1 for v in values.values()):
            raise PortalError(400, "Invalid query parameters")
        return {key: values[key][0] for key in expected}

    def _require_origin(self):
        hosts = self.headers.get_all("Host", [])
        origins = self.headers.get_all("Origin", [])
        if len(hosts) != 1 or len(origins) != 1:
            raise PortalError(403, "A matching Origin and Host are required")
        try:
            origin = urlsplit(origins[0])
            valid = (origin.scheme in ("http", "https") and origin.netloc == hosts[0]
                     and not origin.path and not origin.query and not origin.fragment
                     and origin.username is None and origin.password is None)
        except ValueError:
            valid = False
        if not valid:
            raise PortalError(403, "Origin must match Host exactly")

    def _body_length(self, maximum):
        if self.headers.get_all("Transfer-Encoding"):
            raise PortalError(400, "Transfer-Encoding is not supported")
        lengths = self.headers.get_all("Content-Length", [])
        if not lengths:
            raise PortalError(411, "Content-Length is required")
        if (len(lengths) != 1 or not lengths[0].isascii()
                or not lengths[0].isdigit() or len(lengths[0]) > 12):
            raise PortalError(400, "Invalid Content-Length")
        length = int(lengths[0])
        if length > maximum:
            raise PortalError(413, "Request body is too large")
        self.connection.settimeout(30)
        return length

    def _read_body(self, length):
        body = self.rfile.read(length)
        if len(body) != length:
            raise PortalError(400, "Incomplete request body")
        return body

    def _files(self):
        result = {"truncated": False}
        for area in AREAS:
            files = []
            fd = open_area(area)
            try:
                with os.scandir(fd) as entries:
                    for count, entry in enumerate(entries):
                        if count >= MAX_LIST_ENTRIES:
                            result["truncated"] = True
                            break
                        try:
                            valid_name(entry.name)
                            info = entry.stat(follow_symlinks=False)
                            if stat.S_ISREG(info.st_mode):
                                files.append({"name": entry.name, "size": info.st_size,
                                              "url": "/api/download?" + urlencode(
                                                  {"area": area, "name": entry.name})})
                        except (PortalError, FileNotFoundError):
                            continue
            finally:
                os.close(fd)
            result[area] = sorted(files, key=lambda item: item["name"])
        self._json(200, result)

    def _download(self, params):
        name = valid_name(params["name"])
        directory = open_area(params["area"])
        try:
            try:
                fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                             dir_fd=directory)
            except OSError as error:
                if error.errno in (errno.ENOENT, errno.ELOOP, errno.EACCES):
                    raise PortalError(404, "Regular file not found") from None
                raise
        finally:
            os.close(directory)
        with os.fdopen(fd, "rb") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode):
                raise PortalError(404, "Regular file not found")
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(info.st_size))
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Disposition", "attachment; filename=\"download\"; "
                             "filename*=UTF-8''" + quote(name, safe=""))
            self.end_headers()
            if self.command != "HEAD":
                remaining = info.st_size
                while remaining:
                    chunk = source.read(min(CHUNK, remaining))
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    remaining -= len(chunk)

    def _upload(self, query):
        name = valid_name(self._params(query, ("name",))["name"])
        length = self._body_length(MAX_UPLOAD)
        directory = open_area("imports")
        temporary = TEMP_PREFIX + secrets.token_hex(16)
        created = False
        try:
            # Fail early, then link atomically below to also handle races.
            try:
                os.stat(name, dir_fd=directory, follow_symlinks=False)
            except FileNotFoundError:
                pass
            else:
                raise PortalError(409, "Filename already exists; choose another name")
            fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600, dir_fd=directory)
            created = True
            with os.fdopen(fd, "wb") as destination:
                remaining = length
                while remaining:
                    chunk = self._read_body(min(CHUNK, remaining))
                    destination.write(chunk)
                    remaining -= len(chunk)
            try:
                os.link(temporary, name, src_dir_fd=directory, dst_dir_fd=directory,
                        follow_symlinks=False)
            except FileExistsError:
                raise PortalError(409, "Filename already exists; choose another name") from None
        finally:
            try:
                if created:
                    os.unlink(temporary, dir_fd=directory)
            finally:
                os.close(directory)
        self._json(201, {"name": name, "size": length, "path": "/data/imports/" + name})

    def _clipboard(self, query):
        self._params(query, ())
        if (len(self.headers.get_all("Content-Type", [])) != 1
                or self.headers.get_content_type() != "application/json"
                or self.headers.get_param("charset", "utf-8").lower() != "utf-8"):
            raise PortalError(415, "Use application/json with UTF-8")
        body = self._read_body(self._body_length(MAX_CLIPBOARD))
        try:
            value = json.loads(body.decode("utf-8"))
            if not isinstance(value, dict) or set(value) != {"text"} or not isinstance(value["text"], str):
                raise ValueError()
            value["text"].encode("utf-8")
        except (ValueError, UnicodeError, RecursionError):
            raise PortalError(400, "Expected JSON object with a text string") from None
        try:
            copy_clipboard(value["text"])
        except (OSError, subprocess.SubprocessError):
            raise PortalError(503, "Clipboard unavailable; check xclip and DISPLAY") from None
        self._json(200, {"ok": True})

    def do_GET(self):
        try:
            route = self._route()
            assets = {"/": ("index.html", "text/html; charset=utf-8"),
                      "/portal.js": ("portal.js", "text/javascript; charset=utf-8"),
                      "/portal.css": ("portal.css", "text/css; charset=utf-8")}
            if route.path in assets:
                filename, content_type = assets[route.path]
                self._reply(200, (self.portal_root / filename).read_bytes(), content_type,
                            (("Content-Security-Policy", CSP), ("X-Frame-Options", "DENY")))
            elif route.path == "/api/files":
                self._params(route.query, ())
                self._files()
            elif route.path == "/api/download":
                self._download(self._params(route.query, ("area", "name")))
            elif route.path.startswith("/api/"):
                raise PortalError(404, "Unknown API")
            elif self.command == "HEAD":
                super().do_HEAD()
            else:
                # Keep websockify's noVNC static handling. Its handle_one_request
                # intercepts WebSocket upgrades before this method is called.
                super().do_GET()
        except PortalError as error:
            self._error(error)
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
        except OSError:
            self._error(PortalError(500, "Portal storage is unavailable"))

    def do_HEAD(self):
        self.do_GET()

    def do_POST(self):
        try:
            self._require_origin()
            route = self._route()
            if route.path == "/api/upload":
                self._upload(route.query)
            elif route.path == "/api/clipboard":
                self._clipboard(route.query)
            else:
                raise PortalError(404, "Unknown API")
        except PortalError as error:
            self._error(error)
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
        except TimeoutError:
            self._error(PortalError(408, "Request body timed out"))
        except OSError:
            self._error(PortalError(500, "Portal storage is unavailable"))


def main():
    for directory in AREAS.values():
        os.makedirs(directory, exist_ok=True)
    print("WARNING: no authentication or TLS; trusted LAN only. Do not expose port 6080 to the Internet.",
          flush=True)
    WebSocketProxy(
        RequestHandlerClass=PortalHandler,
        listen_host="0.0.0.0", listen_port=6080,
        target_host="127.0.0.1", target_port=5900,
        web="/usr/share/novnc", file_only=True,
    ).start_server()


if __name__ == "__main__":
    main()
