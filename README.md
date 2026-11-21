# piperepo

piperepo serves one package repository from S3. Each instance serves one
format: apt, arch (pacman), apk or rpm. It takes uploads over HTTP, signs
every index and serves the tree read-only.

This is work in progress. The format readers, index writers and signing
are written and have unit tests. The repository core and the HTTP API are
written but not tested yet, and nothing has been run against the real
clients so far.

## Run

    piperepo serve --format apt --config piperepo.yaml

```yaml
listen: 127.0.0.1:8080
s3: {endpoint: http://127.0.0.1:8333, region: us-east-1, bucket: debian, access_key: k, secret_key: s, path_style: true}
token_file: /run/secrets/token   # every request but GET /health needs it
seed_file: /run/secrets/seed     # or key_file: an armored OpenPGP key, or a PEM RSA key for apk
```

## HTTP

- `GET /health`
- `PUT /upload/<file>` stores a package and publishes an index that lists
  it. It answers 201 with `Piperepo-Name`, `Piperepo-Version` and
  `Piperepo-Arch`.
- `GET /api/packages[?name=]` and `GET /api/packages/<name>/<version>`
  list packages; the second also gives each package's files.
- `DELETE /api/packages/<file>`
- Anything else is the published tree.

## Storage

Packages live at `packages/<file>`. Beside each is `meta/<file>.json`,
which is remade from the package when it is missing. Every rebuild writes a
new generation under `index/<gen>/`, then `pointer.json`. Several
instances can share one bucket, with no locks.
