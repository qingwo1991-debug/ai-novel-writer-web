import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('bootstrap', Path(__file__).with_name('bootstrap.py'))
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class BootstrapTests(unittest.TestCase):
    def test_initial_seed_and_preserve_settings(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / 'app'
            result = bootstrap.seed(root)
            self.assertEqual(result['locale'], 'zh-CN')
            self.assertEqual(result['dialogDirectories']['project-create'], '/data/workspaces')
            self.assertEqual(result['proxy']['host'], '192.168.1.15')
            result['locale'] = 'en-US'
            result['proxy']['enabled'] = False
            result['dialogDirectories']['project-create'] = '/data/my-books'
            (root / 'config.json').write_text(json.dumps(result))
            again = bootstrap.seed(root)
            self.assertEqual(again, result)

    def test_committed_generation(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            generation = '84d568e3-6080-4e4f-8030-6e5cba73c746'
            (root / '.migration').mkdir()
            (root / '.migration' / 'receipt.json').write_text(json.dumps({'completed': True, 'generation': generation}))
            directory = root / 'generations' / generation
            directory.mkdir(parents=True)
            bootstrap.seed(root)
            self.assertTrue((directory / 'config.json').is_file())
            self.assertFalse((root / 'config.json').exists())

    def test_bad_config_preserved(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / 'config.json').write_text('broken')
            with self.assertRaises(ValueError):
                bootstrap.seed(root)
            self.assertEqual((root / 'config.json').read_text(), 'broken')

    def test_bad_generation_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / '.migration').mkdir()
            (root / '.migration' / 'receipt.json').write_text(json.dumps({'completed': True, 'generation': '../escape'}))
            with self.assertRaises(ValueError):
                bootstrap.seed(root)


if __name__ == '__main__':
    unittest.main()
