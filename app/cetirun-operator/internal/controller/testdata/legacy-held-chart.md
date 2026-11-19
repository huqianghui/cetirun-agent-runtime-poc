# Isolated held-preparation regression fixture

`legacy-held-chart-cfdf7c55.tgz` contains the unchanged runtime Chart sources at
[cfdf7c5550484b7290eb268a8ffb76f9b87058c3](https://github.com/Azure/cetirun/tree/cfdf7c5550484b7290eb268a8ffb76f9b87058c3/app/cetirun-chart).
Its SHA-256 is:

```text
b4916f6e804d0d56cc137799934fce17eebc6b70b87ef0d38680a319f39a79e2
```

Only the held-preparation tests that already exist use this fixture. They use
it to isolate their authorization, receipts, fencing, and cleanup assertions
from runtime Chart changes. That held test contract does not support Hermes
Gateway bindings. The current native execution tests use the current source
Chart. This is a test fixture only. The platform supports only the current
Gateway contract.

Do not refresh this fixture from changed output to make a regression pass.
Python 3 and Git are necessary, and the commit must already be present
locally. To reproduce the fixture from the public source commit, run this
script at the repository root:

```bash
python3 - <<'PY'
import gzip
import hashlib
import io
import pathlib
import subprocess
import tarfile

revision = 'cfdf7c5550484b7290eb268a8ffb76f9b87058c3'
root = 'app/cetirun-chart/'
entries = subprocess.check_output(
    ['git', 'ls-tree', '-r', '-z', revision, '--', root]
).split(b'\0')
archive = pathlib.Path(
    'app/cetirun-operator/internal/controller/testdata/legacy-held-chart-cfdf7c55.tgz'
)
with archive.open('wb') as output:
    with gzip.GzipFile(filename='', mode='wb', fileobj=output, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as tar:
            for entry in entries:
                if not entry:
                    continue
                metadata, path = entry.decode().split('\t', 1)
                mode, kind, sha = metadata.split()
                assert kind == 'blob' and mode in ('100644', '100755')
                data = subprocess.check_output(['git', 'cat-file', 'blob', sha])
                info = tarfile.TarInfo('cetirun/' + path.removeprefix(root))
                info.mode = int(mode, 8) & 0o777
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
assert hashlib.sha256(archive.read_bytes()).hexdigest() == (
    'b4916f6e804d0d56cc137799934fce17eebc6b70b87ef0d38680a319f39a79e2'
)
PY
```
