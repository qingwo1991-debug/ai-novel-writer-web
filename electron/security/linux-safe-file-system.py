#!/usr/bin/env python3
"""Linux capability helper; JSON-lines protocol shared with the native helpers.

Every lookup is relative to held directory descriptors and uses O_NOFOLLOW.
Writes keep the root, parent chain, original target and staged file open across
ready/commit. renameat2 is required: NOREPLACE protects creation and EXCHANGE
preserves the existing-target invariant. There is deliberately no pathname-I/O
fallback. This does not promise an inode-conditional rename against a hostile
process continuously mutating the same directory: Linux has no such syscall.
"""

import base64
import binascii
import ctypes
import errno
import json
import os
import re
import stat
import sys
import uuid
from contextlib import ExitStack

MAX_PATH_CHARACTERS = 32_000
MAX_SEGMENTS = 256
MAX_TEXT_BYTES = 64 * 1024 * 1024
MAX_DIRECTORY_ENTRIES = 16_384
MAX_MESSAGE_BYTES = (MAX_TEXT_BYTES * 14 + 9) // 10 + 2 * 1024 * 1024
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
FILE_FLAGS = os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW | os.O_CLOEXEC
RESERVED = re.compile(r"^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$", re.I)
DECIMAL = re.compile(r"^(?:0|[1-9][0-9]*)$")


class SecureFsError(Exception):
    pass


def fail(code):
    raise SecureFsError("SECURE_FS_" + code)


def error_code(error):
    if error.errno == errno.ELOOP:
        return "SECURE_FS_REPARSE_POINT"
    if error.errno in (errno.ENOENT, errno.ENOTDIR):
        return "SECURE_FS_NOT_FOUND"
    return "SECURE_FS_OPEN_FAILED"


def read_message(limit=MAX_MESSAGE_BYTES):
    line = sys.stdin.buffer.readline(limit + 1)
    if len(line) > limit:
        fail("REQUEST_TOO_LARGE")
    if not line:
        fail("INVALID_PATH")
    try:
        request = json.loads(line)
    except (ValueError, UnicodeError, RecursionError):
        fail("INVALID_PATH")
    if not isinstance(request, dict):
        fail("INVALID_PATH")
    return request


def respond(response):
    # ASCII escaping also avoids surrogate/terminal interpretation of filenames.
    encoded = json.dumps(response, ensure_ascii=True, separators=(",", ":"))
    if len(encoded) > MAX_MESSAGE_BYTES:
        encoded = '{"ok":false,"code":"SECURE_FS_HELPER_INVALID_RESPONSE"}'
    sys.stdout.write(encoded + "\n")
    sys.stdout.flush()


def safe_segment(segment):
    return (isinstance(segment, str) and bool(segment)
            and segment not in (".", "..")
            and not any(character in segment for character in "\0:/\\")
            and not segment.endswith((".", " "))
            and RESERVED.fullmatch(segment) is None
            and not any(0xD800 <= ord(character) <= 0xDFFF for character in segment))


def relative_segments(value):
    if (not isinstance(value, str) or len(value) > MAX_PATH_CHARACTERS
            or "\0" in value or value.startswith(("/", "\\"))):
        fail("INVALID_PATH")
    segments = [part for part in re.split(r"[/\\]+", value) if part not in ("", ".")]
    if len(segments) > MAX_SEGMENTS or not all(map(safe_segment, segments)):
        fail("INVALID_PATH")
    return segments


def root_segments(value):
    if (not isinstance(value, str) or not value.startswith("/")
            or len(value) > MAX_PATH_CHARACTERS or "\0" in value):
        fail("INVALID_PATH")
    segments = [part for part in value.split("/") if part]
    if (len(segments) > MAX_SEGMENTS or any(part in (".", "..") for part in segments)
            or any(0xD800 <= ord(character) <= 0xDFFF for character in value)):
        fail("INVALID_PATH")
    return segments


def identity(information):
    return information.st_dev, information.st_ino


def parse_identity(value):
    if not isinstance(value, dict) or set(value) != {"volumeSerialNumber", "fileIndex"}:
        fail("INVALID_PATH")
    result = []
    for key, maximum in (("volumeSerialNumber", 2**32 - 1), ("fileIndex", 2**64 - 1)):
        number = value[key]
        if (not isinstance(number, str) or len(number) > 20
                or not DECIMAL.fullmatch(number) or int(number) > maximum):
            fail("INVALID_PATH")
        result.append(int(number))
    return tuple(result)


def lstat_at(parent, name):
    try:
        return os.stat(name, dir_fd=parent, follow_symlinks=False)
    except FileNotFoundError:
        return None


def require_safe_file(information):
    if stat.S_ISLNK(information.st_mode):
        fail("REPARSE_POINT")
    if not stat.S_ISREG(information.st_mode) or information.st_nlink != 1:
        fail("OPEN_FAILED")


def require_safe_entry(information):
    if stat.S_ISLNK(information.st_mode):
        fail("REPARSE_POINT")
    if stat.S_ISDIR(information.st_mode):
        return
    require_safe_file(information)


class Resolver:
    """Hold every directory, including the root, until the operation settles."""

    def __init__(self, request):
        self.absolute = root_segments(request.get("rootPath"))
        self.expected = parse_identity(request.get("rootIdentity"))
        self.relative = relative_segments(request.get("relativePath"))
        self.stack = ExitStack()
        try:
            self.root = self.open_root(self.stack)
            self.parents = [((), self.root)]
        except BaseException:
            self.stack.close()
            raise

    def __enter__(self):
        return self

    def __exit__(self, *arguments):
        return self.stack.__exit__(*arguments)

    @staticmethod
    def hold(stack, descriptor):
        stack.callback(os.close, descriptor)
        return descriptor

    @staticmethod
    def open_directory(parent, name, stack, create=False):
        existing = lstat_at(parent, name)
        if existing is not None and stat.S_ISLNK(existing.st_mode):
            fail("REPARSE_POINT")
        try:
            descriptor = os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
        except FileNotFoundError:
            if not create:
                raise
            try:
                os.mkdir(name, mode=0o755, dir_fd=parent)
            except FileExistsError:
                pass
            descriptor = os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
        return Resolver.hold(stack, descriptor)

    def open_root(self, stack):
        descriptor = self.hold(stack, os.open("/", DIRECTORY_FLAGS))
        for segment in self.absolute:
            descriptor = self.open_directory(descriptor, segment, stack)
        if identity(os.fstat(descriptor)) != self.expected:
            fail("ROOT_CHANGED")
        return descriptor

    def directory(self, segments, create=False):
        descriptor = self.root
        prefix = []
        for segment in segments:
            descriptor = self.open_directory(descriptor, segment, self.stack, create)
            prefix.append(segment)
            self.parents.append((tuple(prefix), descriptor))
        return descriptor

    def parent(self):
        if not self.relative:
            fail("INVALID_PATH")
        return self.directory(self.relative[:-1]), self.relative[-1]

    def revalidate(self):
        # Rewalk both the externally named root and each held relative parent.
        # An ancestor renamed/replaced during beforeReplace must not be written.
        with ExitStack() as check:
            try:
                descriptor = self.open_root(check)
                previous = ()
                for segments, original in self.parents:
                    if segments != previous:
                        # directory() records one contiguous chain; reopening it
                        # once bounds descriptors to O(depth), not O(depth²).
                        if segments[:-1] != previous:
                            fail("ROOT_CHANGED")
                        descriptor = self.open_directory(descriptor, segments[-1], check)
                    if identity(os.fstat(descriptor)) != identity(os.fstat(original)):
                        fail("ROOT_CHANGED")
                    previous = segments
            except OSError:
                fail("ROOT_CHANGED")

    def regular(self, parent, leaf):
        existing = lstat_at(parent, leaf)
        if existing is None:
            fail("NOT_FOUND")
        require_safe_file(existing)
        descriptor = self.hold(self.stack, os.open(leaf, FILE_FLAGS, dir_fd=parent))
        opened = os.fstat(descriptor)
        require_safe_file(opened)
        if identity(opened) != identity(existing):
            fail("OPEN_FAILED")
        return descriptor


def read_file(resolver, request):
    maximum = request.get("maxBytes", MAX_TEXT_BYTES)
    if type(maximum) is not int or maximum < 0 or maximum > MAX_TEXT_BYTES:
        fail("INVALID_OPERATION")
    parent, leaf = resolver.parent()
    descriptor = resolver.regular(parent, leaf)
    if os.fstat(descriptor).st_size > maximum:
        fail("FILE_TOO_LARGE")
    content = bytearray()
    while True:
        chunk = os.read(descriptor, min(64 * 1024, maximum - len(content) + 1))
        if not chunk:
            break
        content.extend(chunk)
        if len(content) > maximum:
            fail("FILE_TOO_LARGE")
    require_safe_file(os.fstat(descriptor))
    resolver.revalidate()
    return {"ok": True, "contentBase64": base64.b64encode(content).decode("ascii")}


def exists(resolver):
    if not resolver.relative:
        return {"ok": True, "exists": True}
    try:
        parent, leaf = resolver.parent()
        information = lstat_at(parent, leaf)
    except FileNotFoundError:
        return {"ok": True, "exists": False}
    except NotADirectoryError:
        return {"ok": True, "exists": False}
    if information is not None:
        require_safe_entry(information)
    resolver.revalidate()
    return {"ok": True, "exists": information is not None}


def list_directory(resolver):
    directory = resolver.directory(resolver.relative)
    entries = []
    # scandir(fd) binds enumeration to the opened directory, not a host path.
    with os.scandir(directory) as stream:
        for entry in stream:
            if len(entries) >= MAX_DIRECTORY_ENTRIES:
                fail("DIRECTORY_TOO_LARGE")
            if not safe_segment(entry.name):
                fail("OPEN_FAILED")
            information = os.stat(entry.name, dir_fd=directory, follow_symlinks=False)
            require_safe_entry(information)
            entries.append({"name": entry.name, "isDirectory": stat.S_ISDIR(information.st_mode)})
    resolver.revalidate()
    return {"ok": True, "entries": entries}


def rename_at(parent, source, destination, flags):
    # libc exposes renameat2 on the Debian image. Unsupported kernels/filesystems
    # fail closed rather than weakening NOREPLACE/EXCHANGE to a check+rename.
    library = ctypes.CDLL(None, use_errno=True)
    function = getattr(library, "renameat2", None)
    if function is None:
        fail("WRITE_FAILED")
    function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    function.restype = ctypes.c_int
    if function(parent, os.fsencode(source), parent, os.fsencode(destination), flags) != 0:
        fail("WRITE_FAILED")


def same_entry(parent, name, expected):
    current = lstat_at(parent, name)
    return current is not None and identity(current) == expected


def decode_content(request):
    encoded = request.get("contentBase64")
    if not isinstance(encoded, str):
        fail("INVALID_TEXT")
    if len(encoded) > ((MAX_TEXT_BYTES + 2) // 3) * 4:
        fail("FILE_TOO_LARGE")
    try:
        content = base64.b64decode(encoded, validate=True)
    except (ValueError, binascii.Error):
        fail("INVALID_TEXT")
    if len(content) > MAX_TEXT_BYTES:
        fail("FILE_TOO_LARGE")
    return content


def write_atomically(resolver, request):
    content = decode_content(request)
    required = request.get("mustAlreadyExist", False)
    if type(required) is not bool:
        fail("INVALID_OPERATION")
    parent, leaf = resolver.parent()
    initial = lstat_at(parent, leaf)
    expected = None
    if initial is not None:
        original = resolver.regular(parent, leaf)
        expected = identity(os.fstat(original))
    elif required:
        fail("NOT_FOUND")

    temporary_name = ".ai-novel-" + uuid.uuid4().hex + ".tmp"
    temporary = resolver.hold(resolver.stack, os.open(
        temporary_name, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
        0o600, dir_fd=parent))
    staged_identity = identity(os.fstat(temporary))
    cleanup_identity = staged_identity
    try:
        os.fchmod(temporary, 0o600)
        remaining = memoryview(content)
        while remaining:
            written = os.write(temporary, remaining)
            if written <= 0:
                fail("WRITE_FAILED")
            remaining = remaining[written:]
        os.fsync(temporary)
        respond({"ok": True, "phase": "ready"})
        try:
            command = read_message(4096)
        except SecureFsError:
            fail("CANCELLED")
        if command.get("command") != "commit":
            fail("CANCELLED")

        resolver.revalidate()
        if not same_entry(parent, temporary_name, staged_identity):
            fail("WRITE_FAILED")
        require_safe_file(os.fstat(temporary))
        current = lstat_at(parent, leaf)
        if expected is None:
            if current is not None:
                fail("WRITE_FAILED")
            rename_at(parent, temporary_name, leaf, 1)  # RENAME_NOREPLACE
            cleanup_identity = None
        else:
            if current is None:
                fail("WRITE_FAILED")
            require_safe_file(current)
            if identity(current) != expected:
                fail("WRITE_FAILED")
            # EXCHANGE cannot silently recreate a concurrently removed target.
            # Inspect the displaced inode; preserve it if a competing writer won.
            rename_at(parent, temporary_name, leaf, 2)  # RENAME_EXCHANGE
            cleanup_identity = None
            displaced = lstat_at(parent, temporary_name)
            if (displaced is None or identity(displaced) != expected
                    or not stat.S_ISREG(displaced.st_mode) or displaced.st_nlink != 1):
                if displaced is not None and same_entry(parent, leaf, staged_identity):
                    try:
                        rename_at(parent, temporary_name, leaf, 2)
                        cleanup_identity = staged_identity
                    except SecureFsError:
                        pass  # Do not delete the displaced entry if rollback failed.
                fail("WRITE_FAILED")
            cleanup_identity = expected
        os.fsync(parent)
        resolver.revalidate()
        return {"ok": True}
    finally:
        # Never remove an unrelated entry substituted for our temporary name.
        if cleanup_identity is not None and same_entry(parent, temporary_name, cleanup_identity):
            os.unlink(temporary_name, dir_fd=parent)
            os.fsync(parent)


def dispatch(request):
    operation = request.get("operation")
    if operation not in ("read", "write", "mkdir", "exists", "list"):
        fail("INVALID_OPERATION")
    with Resolver(request) as resolver:
        if operation == "read":
            return read_file(resolver, request)
        if operation == "write":
            return write_atomically(resolver, request)
        if operation == "exists":
            return exists(resolver)
        if operation == "list":
            return list_directory(resolver)
        resolver.directory(resolver.relative, create=True)
        resolver.revalidate()
        return {"ok": True}


def main():
    try:
        response = dispatch(read_message())
    except SecureFsError as error:
        response = {"ok": False, "code": str(error)}
    except OSError as error:
        response = {"ok": False, "code": error_code(error)}
    except (ValueError, TypeError, UnicodeError, RecursionError):
        response = {"ok": False, "code": "SECURE_FS_INVALID_PATH"}
    except Exception:
        response = {"ok": False, "code": "SECURE_FS_HELPER_FAILED"}
    respond(response)


if __name__ == "__main__":
    main()
