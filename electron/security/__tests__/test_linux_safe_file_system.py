"""Focused Linux helper protocol and capability-safety tests (stdlib only)."""

import base64
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HELPER = Path(__file__).resolve().parents[1] / "linux-safe-file-system.py"
SPEC = importlib.util.spec_from_file_location("linux_safe_file_system", HELPER)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


@unittest.skipUnless(sys.platform == "linux", "Linux descriptor operations required")
class LinuxSafeFileSystemTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "project"
        self.root.mkdir()

    def request(self, operation, relative="", **extra):
        info = self.root.stat()
        return {"operation": operation, "rootPath": str(self.root), "relativePath": relative,
                "rootIdentity": {"volumeSerialNumber": str(info.st_dev), "fileIndex": str(info.st_ino)},
                **extra}

    def invoke(self, request):
        completed = subprocess.run([sys.executable, str(HELPER)], input=json.dumps(request) + "\n",
                                   text=True, capture_output=True, timeout=5, check=True)
        self.assertEqual(completed.stderr, "")
        lines = completed.stdout.splitlines()
        self.assertEqual(len(lines), 1)
        return json.loads(lines[0])

    def write(self, relative, content=b"new", before_replace=None, command="commit", **extra):
        request = self.request("write", relative,
                               contentBase64=base64.b64encode(content).decode(), **extra)
        child = subprocess.Popen([sys.executable, str(HELPER)], text=True,
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            child.stdin.write(json.dumps(request) + "\n")
            child.stdin.flush()
            ready = json.loads(child.stdout.readline())
            if not ready.get("ok"):
                child.stdin.close()
                child.stdin = None
                stdout, stderr = child.communicate(timeout=5)
                self.assertEqual((stdout, stderr, child.returncode), ("", "", 0))
                return ready
            self.assertEqual(ready, {"ok": True, "phase": "ready"})
            if before_replace:
                before_replace()
            stdout, stderr = child.communicate(json.dumps({"command": command}) + "\n", timeout=5)
            self.assertEqual((stderr, child.returncode), ("", 0))
            return json.loads(stdout)
        finally:
            if child.poll() is None:
                child.kill()
                child.communicate(timeout=5)

    def assert_failure(self, result, code=None):
        self.assertIs(result.get("ok"), False)
        self.assertTrue(result.get("code", "").startswith("SECURE_FS_"))
        if code:
            self.assertEqual(result["code"], "SECURE_FS_" + code)

    def test_happy_read_write_mkdir_list_exists_and_unicode(self):
        self.assertEqual(self.invoke(self.request("mkdir", "正文\\卷一")), {"ok": True})
        text = "第一章：中文与 Unicode 😀\n".encode()
        self.assertEqual(self.write("正文\\卷一\\第一章.txt", text), {"ok": True})
        target = self.root / "正文" / "卷一" / "第一章.txt"
        self.assertEqual(target.read_bytes(), text)
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.invoke(self.request("read", "正文/卷一/第一章.txt", maxBytes=1024)),
                         {"ok": True, "contentBase64": base64.b64encode(text).decode()})
        self.assertEqual(self.invoke(self.request("exists", "正文/卷一/第一章.txt")),
                         {"ok": True, "exists": True})
        self.assertEqual(self.invoke(self.request("exists", "missing/nothing")),
                         {"ok": True, "exists": False})
        self.assertEqual(self.invoke(self.request("list", "正文/卷一")),
                         {"ok": True, "entries": [{"name": "第一章.txt", "isDirectory": False}]})
        self.assertEqual(self.invoke(self.request("list")),
                         {"ok": True, "entries": [{"name": "正文", "isDirectory": True}]})
        self.assertEqual(self.write("正文/卷一/第一章.txt", b"replacement", mustAlreadyExist=True),
                         {"ok": True})
        self.assertEqual(target.read_bytes(), b"replacement")
        self.assertEqual(list(self.root.rglob("*.tmp")), [])

    def test_traversal_and_absolute_paths_rejected(self):
        for relative in ("../outside", "dir\\..\\outside", "/etc/passwd", "\\etc\\passwd",
                         "C:\\file", "bad\0name", "con", "file. "):
            with self.subTest(relative=relative):
                self.assert_failure(self.invoke(self.request("exists", relative)), "INVALID_PATH")

    def test_invalid_protocol_identity_and_read_limits(self):
        (self.root / "chapter").write_bytes(b"abc")
        for identity in ({"volumeSerialNumber": "00", "fileIndex": "1"},
                         {"volumeSerialNumber": "1", "fileIndex": "-1"}, None):
            request = self.request("exists")
            request["rootIdentity"] = identity
            self.assert_failure(self.invoke(request), "INVALID_PATH")
        for maximum in (-1, True, 1.5, MODULE.MAX_TEXT_BYTES + 1):
            self.assert_failure(self.invoke(self.request("read", "chapter", maxBytes=maximum)),
                                "INVALID_OPERATION")
        self.assert_failure(self.invoke(self.request("read", "chapter", maxBytes=2)), "FILE_TOO_LARGE")
        (self.root / "empty").touch()
        self.assertEqual(self.invoke(self.request("read", "empty", maxBytes=0)),
                         {"ok": True, "contentBase64": ""})
        self.assert_failure(self.invoke(self.request("write", "x", contentBase64="!!!")), "INVALID_TEXT")

    def test_symlink_leaf_parent_and_root_ancestor_rejected(self):
        outside = Path(self.temporary.name) / "outside"
        outside.mkdir()
        (outside / "secret").write_bytes(b"private")
        (self.root / "link").symlink_to(outside / "secret")
        for operation in ("read", "exists"):
            self.assert_failure(self.invoke(self.request(operation, "link")), "REPARSE_POINT")
        self.assert_failure(self.write("link"), "REPARSE_POINT")
        self.assert_failure(self.invoke(self.request("list")), "REPARSE_POINT")
        (self.root / "link").unlink()
        (self.root / "parent").symlink_to(outside, target_is_directory=True)
        self.assert_failure(self.invoke(self.request("read", "parent/secret")), "REPARSE_POINT")
        self.assert_failure(self.invoke(self.request("mkdir", "parent/subdir")), "REPARSE_POINT")
        alias = Path(self.temporary.name) / "alias"
        alias.symlink_to(self.root.parent, target_is_directory=True)
        request = self.request("exists")
        request["rootPath"] = str(alias / self.root.name)
        self.assert_failure(self.invoke(request), "REPARSE_POINT")
        self.assertEqual((outside / "secret").read_bytes(), b"private")

    def test_hardlinked_regular_files_rejected(self):
        outside = Path(self.temporary.name) / "outside-file"
        outside.write_bytes(b"private")
        os.link(outside, self.root / "linked")
        for operation in ("read", "exists"):
            self.assert_failure(self.invoke(self.request(operation, "linked")), "OPEN_FAILED")
        self.assert_failure(self.write("linked"), "OPEN_FAILED")
        self.assert_failure(self.invoke(self.request("list")), "OPEN_FAILED")
        self.assertEqual(outside.read_bytes(), b"private")

    def test_root_identity_mismatch_rejected(self):
        request = self.request("exists")
        request["rootIdentity"]["fileIndex"] = str(int(request["rootIdentity"]["fileIndex"]) + 1)
        self.assert_failure(self.invoke(request), "ROOT_CHANGED")

    def test_root_replacement_at_ready_is_rejected(self):
        (self.root / "chapter").write_bytes(b"old")
        moved = self.root.with_name("moved-root")

        def replace():
            self.root.rename(moved)
            self.root.mkdir()
            (self.root / "chapter").write_bytes(b"other")

        self.assert_failure(self.write("chapter", before_replace=replace, mustAlreadyExist=True), "ROOT_CHANGED")
        self.assertEqual((moved / "chapter").read_bytes(), b"old")
        self.assertEqual((self.root / "chapter").read_bytes(), b"other")
        self.assertEqual(list(moved.glob("*.tmp")), [])

    def test_parent_replacement_at_ready_is_rejected(self):
        parent = self.root / "parent"
        parent.mkdir()
        (parent / "chapter").write_bytes(b"old")

        def replace():
            parent.rename(self.root / "moved-parent")
            parent.mkdir()
            (parent / "chapter").write_bytes(b"other")

        self.assert_failure(self.write("parent/chapter", before_replace=replace), "ROOT_CHANGED")
        self.assertEqual((self.root / "moved-parent" / "chapter").read_bytes(), b"old")
        self.assertEqual((parent / "chapter").read_bytes(), b"other")

    def test_before_replace_cancellation_preserves_original(self):
        (self.root / "chapter").write_bytes(b"old")
        self.assert_failure(self.write("chapter", command="cancel"), "CANCELLED")
        self.assertEqual((self.root / "chapter").read_bytes(), b"old")
        self.assertEqual(list(self.root.glob("*.tmp")), [])

    def test_must_already_exist_missing_or_removed_at_ready(self):
        self.assert_failure(self.write("chapter", mustAlreadyExist=True), "NOT_FOUND")
        (self.root / "chapter").write_bytes(b"old")
        self.assert_failure(self.write("chapter", mustAlreadyExist=True,
                                       before_replace=lambda: (self.root / "chapter").unlink()), "WRITE_FAILED")
        self.assertFalse((self.root / "chapter").exists())
        self.assertEqual(list(self.root.glob("*.tmp")), [])

    def test_target_replaced_by_symlink_or_hardlink_at_ready(self):
        outside = Path(self.temporary.name) / "outside-file"
        outside.write_bytes(b"private")
        for hardlink in (False, True):
            target = self.root / "chapter"
            target.write_bytes(b"old")

            def replace():
                target.unlink()
                if hardlink:
                    os.link(outside, target)
                else:
                    target.symlink_to(outside)

            self.assert_failure(self.write("chapter", before_replace=replace, mustAlreadyExist=True))
            self.assertEqual(outside.read_bytes(), b"private")
            target.unlink()

    def test_exchange_failure_preserves_existing_and_cleans_stage(self):
        target = self.root / "chapter"
        target.write_bytes(b"old")
        request = self.request("write", "chapter", contentBase64="bmV3", mustAlreadyExist=True)
        with MODULE.Resolver(request) as resolver, mock.patch.object(MODULE, "respond"), \
                mock.patch.object(MODULE, "read_message", return_value={"command": "commit"}), \
                mock.patch.object(MODULE, "rename_at", side_effect=MODULE.SecureFsError("SECURE_FS_WRITE_FAILED")):
            with self.assertRaises(MODULE.SecureFsError):
                MODULE.write_atomically(resolver, request)
        self.assertEqual(target.read_bytes(), b"old")
        self.assertEqual(list(self.root.glob("*.tmp")), [])

    def test_competing_target_at_exchange_is_rolled_back_without_deleting_it(self):
        target = self.root / "chapter"
        target.write_bytes(b"old")
        replacement = self.root / "replacement"
        replacement.write_bytes(b"competitor")
        request = self.request("write", "chapter", contentBase64="bmV3", mustAlreadyExist=True)
        real_rename = MODULE.rename_at
        first = True

        def race(parent, source, destination, flags):
            nonlocal first
            if first:
                first = False
                replacement.replace(target)
            return real_rename(parent, source, destination, flags)

        with MODULE.Resolver(request) as resolver, mock.patch.object(MODULE, "respond"), \
                mock.patch.object(MODULE, "read_message", return_value={"command": "commit"}), \
                mock.patch.object(MODULE, "rename_at", side_effect=race):
            with self.assertRaises(MODULE.SecureFsError):
                MODULE.write_atomically(resolver, request)
        self.assertEqual(target.read_bytes(), b"competitor")
        self.assertEqual(list(self.root.glob("*.tmp")), [])

    def test_directory_entry_limit_is_bounded(self):
        for name in ("a", "b", "c"):
            (self.root / name).touch()
        with MODULE.Resolver(self.request("list")) as resolver, \
                mock.patch.object(MODULE, "MAX_DIRECTORY_ENTRIES", 2):
            with self.assertRaisesRegex(MODULE.SecureFsError, "SECURE_FS_DIRECTORY_TOO_LARGE"):
                MODULE.list_directory(resolver)


if __name__ == "__main__":
    unittest.main()
