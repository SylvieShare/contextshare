#!/usr/bin/env python3
"""Read this VM's ContextShare Lockbox values and exec the service."""
import json
import os
import re
import sys
import urllib.error
import urllib.request
from urllib.parse import urlsplit

direct = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def read_json(url, headers):
    request = urllib.request.Request(url, headers=headers)
    with direct.open(request, timeout=10) as response:
        return json.load(response)


def main():
    secret_id = os.environ.get('CONTEXTSHARE_LOCKBOX_ID', '')
    if len(sys.argv) < 2 or not re.fullmatch(r'[a-z0-9]{15,50}', secret_id):
        print('ContextShare startup configuration is incomplete', file=sys.stderr)
        return 2
    try:
        metadata = 'http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token'
        token = read_json(metadata, {'Metadata-Flavor': 'Google'})['access_token']
        url = f'https://payload.lockbox.api.cloud.yandex.net/lockbox/v1/secrets/{secret_id}/payload'
        payload = read_json(url, {'Authorization': 'Bearer ' + token})
        allowed = {'DB_URL', 'DB_USER', 'DB_PASSWORD'}
        database = {e['key']: e.get('textValue', '') for e in payload['entries'] if e['key'] in allowed}
        if set(database) != allowed or not all(database.values()):
            raise ValueError()
        if urlsplit(database['DB_URL'].removeprefix('jdbc:')).path != '/contextshare':
            raise ValueError()
    except urllib.error.HTTPError as error:
        print(f'ContextShare credentials unavailable (HTTP {error.code})', file=sys.stderr)
        return 2
    except (OSError, KeyError, ValueError, TypeError):
        print('ContextShare credentials or database target are invalid', file=sys.stderr)
        return 2
    os.execve(sys.argv[1], sys.argv[1:], dict(os.environ, **database))


if __name__ == '__main__':
    sys.exit(main())
