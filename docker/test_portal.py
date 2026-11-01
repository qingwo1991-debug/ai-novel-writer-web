import importlib.util
import subprocess
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('portal', Path(__file__).parent / 'portal' / 'server.py')
portal = importlib.util.module_from_spec(spec)
spec.loader.exec_module(portal)


class PortalTests(unittest.TestCase):
    def test_names(self):
        for name in ('../secret', '/etc/passwd', 'a/b', 'a\\b', '', '..', '.portal-upload-bad', 'bad\nheader'):
            with self.subTest(name=name), self.assertRaises(portal.PortalError):
                portal.valid_name(name)
        self.assertEqual(portal.valid_name('小说.txt'), '小说.txt')

    def test_clipboard_utf8(self):
        calls = []
        portal.copy_clipboard('中文测试', run=lambda *args, **kwargs: calls.append((args, kwargs)))
        self.assertEqual(calls[0][1]['input'], '中文测试'.encode())
        self.assertEqual(calls[0][1]['stdout'], subprocess.DEVNULL)
        self.assertIn('UTF8_STRING', calls[0][0][0])


if __name__ == '__main__':
    unittest.main()
