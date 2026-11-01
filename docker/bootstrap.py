#!/usr/bin/env python3
"""Seed container-only preferences without replacing existing author settings."""
import json
import os
import tempfile
from pathlib import Path


def config_path(root: Path) -> Path:
    receipt = root / '.migration' / 'receipt.json'
    if not receipt.exists():
        # The application will migrate this initial config into a generation.
        return root / 'config.json'
    state = json.loads(receipt.read_text(encoding='utf-8'))
    generation = state['generation']
    import re
    if not state.get('completed') or not re.fullmatch(r'[a-f0-9-]{36}', generation):
        raise ValueError('Invalid app migration receipt; refusing to change settings')
    directory = root / 'generations' / generation
    if not directory.is_dir():
        raise ValueError('Committed app generation is missing')
    return directory / 'config.json'


def seed(root: Path) -> dict:
    target = config_path(root)
    config = json.loads(target.read_text(encoding='utf-8')) if target.exists() else {}
    if not isinstance(config, dict):
        raise ValueError('Invalid app config; refusing to replace it')
    defaults = {
        'locale': 'zh-CN', 'theme': 'light', 'defaultModelId': None,
        'autoOpenNextChapterAfterFinalize': False, 'editorFontSize': 16,
        'editorFontFamily': 'Noto Serif SC', 'autoSaveInterval': 30,
        'proxy': {'enabled': True, 'type': 'http', 'host': '192.168.1.15', 'port': 7890},
    }
    for key, value in defaults.items():
        config.setdefault(key, value)
    directories = config.setdefault('dialogDirectories', {})
    if not isinstance(directories, dict):
        raise ValueError('Invalid dialog directories; refusing to replace them')
    for key in ('project-create', 'project-open', 'project-restore-target'):
        directories.setdefault(key, '/data/workspaces')
    for key in ('import-source', 'knowledge-files', 'knowledge-folder', 'legacy-project', 'character-avatar', 'writing-skill'):
        directories.setdefault(key, '/data/imports')
    for key in ('export-directory', 'project-archive'):
        directories.setdefault(key, '/data/exports')
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix='.docker-config-', dir=target.parent)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as stream:
            json.dump(config, stream, ensure_ascii=False, indent=2)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return config


if __name__ == '__main__':
    import shlex
    root = Path(os.environ.get('XDG_CONFIG_HOME', '/data/config')) / 'ai-novel-writer'
    config = seed(root)
    proxy = config.get('proxy') or {}
    # Node 24 fetch requires env-proxy opt-in AND URL before Electron starts.
    # Values are shell-quoted because entrypoint evaluates this output.
    if proxy.get('enabled') and proxy.get('host') and proxy.get('type', 'http') == 'http':
        url = 'http://{}:{}'.format(proxy['host'], int(proxy.get('port', 7890)))
        print('export NODE_USE_ENV_PROXY=1')
        for key in ('HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy'):
            print('export {}={}'.format(key, shlex.quote(url)))
    else:
        print('unset NODE_USE_ENV_PROXY HTTP_PROXY HTTPS_PROXY http_proxy https_proxy')
    bypass = 'localhost,127.0.0.1,::1,192.168.1.15,host.docker.internal'
    print('export NO_PROXY={} no_proxy={}'.format(shlex.quote(bypass), shlex.quote(bypass)))
