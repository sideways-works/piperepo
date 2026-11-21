<img src="docs/brand/banner.svg" alt="piperepo, by Sideways Works" width="400">

# piperepo

piperepo serves one signed package repository from S3. Each instance
serves one format: `apt`, `arch` (pacman), `apk` or `rpm`. Packages come
in through an upload API, and the index is rebuilt and signed after every
upload. The tree is served read-only. There is no database: several
instances can share one bucket, and none waits for another.

## Run

    CGO_ENABLED=0 go build ./cmd/piperepo
    piperepo serve --format apt --config piperepo.yaml

```yaml
listen: 127.0.0.1:8080
s3:
  endpoint: http://127.0.0.1:8333
  region: us-east-1
  bucket: debian
  access_key: ...
  secret_key: ...
  path_style: true
token_file: /run/secrets/token # every request but GET /health needs it
seed_file: /run/secrets/seed   # or key_file
overwrite: false               # a file name taken again with other bytes is 409
max_size: 2147483648           # bytes per package
reconcile: 10s
grace: 10m
origin: piperepo
apt: {suite: stable, component: main, architectures: [amd64, arm64]}
arch: {repo: piperun, architectures: [x86_64, aarch64]}
apk: {key_name: piperun.rsa.pub, architectures: [x86_64, aarch64]}
```

The credential goes as basic authentication's password, with any user, or
as `Authorization: Bearer <token>`.

The signing key comes from `key_file` or from `seed_file`. `key_file` holds
an armored OpenPGP secret key for apt, arch and rpm, or a PEM RSA key for
apk. With `seed_file`, each instance makes the same key from the seed: an
Ed25519 OpenPGP key with a fixed creation time, or a 4096-bit RSA key for
apk. Every instance of one store signs alike, and no key is stored. With
neither, piperepo refuses to start.

## HTTP

- `GET /health`: no credential needed.
- `PUT /upload/<file>`: the package as the body. It answers 201 once an
  index that lists the package is served. Its headers are
  `Piperepo-Name`, `Piperepo-Version`, `Piperepo-Arch` and
  `Piperepo-Size`. A file name that is already taken, with other bytes,
  gets 409. A file of another format gets 400.
- `GET /api/packages[?name=<name>]`: a list of
  `{name, version, arch, file, size, sha256, description, license, uploaded}`.
- `GET /api/packages/<name>/<version>`: one object,
  `{name, version, archs, description, license, size, depends: [{name, range}], files: [{path, size}]}`.
  The files are the first architecture's. A version that isn't there gets
  404.
- `DELETE /api/packages/<file>`: removes the package and rebuilds the index.
- Any other GET is the published tree. Index files are sent with
  `no-cache`. Packages and files named by their hash are sent as
  immutable. Every answer has an ETag.

## The tree

- apt: `dists/<suite>/{InRelease,Release,Release.gpg}`,
  `dists/<suite>/<component>/binary-<arch>/Packages{,.gz}` with by-hash
  copies, and `pool/<component>/<letter>/<name>/<file>`. A package for
  `all` is listed under every architecture. The key is at `key.asc`.
- arch: `<arch>/<repo>.db` and `<arch>/<repo>.files`, each also with a
  `.tar.gz` name, each with a binary `.sig`, and `<arch>/<file>`. A package
  for `any` is in every architecture.
- apk: `<arch>/APKINDEX.tar.gz`, signed inside with `.SIGN.RSA256.<key
  name>`, and `<arch>/<name>-<version>.apk`. A `noarch` package is in every
  architecture and also under `noarch/`, where apk-tools 3 looks for it.
  The key is at `keys/<key name>`.
- rpm: `repodata/repomd.xml` with `repomd.xml.asc`, hash-prefixed
  `primary`, `filelists` and `other`, and `packages/<file>`. The key is at
  `key.asc` and at `repodata/repomd.xml.key`.

## Storage

- `packages/<file>`: each package.
- `meta/<file>.json`: what was read from the package. It is made again
  from the package when it is missing or older than the package, so the
  whole index can always be rebuilt from `packages/` alone.
- `index/<generation>/`: one built index, with a `tree.json` listing every
  path it serves.
- `pointer.json`: the generation that is served.

A rebuild lists `packages/`, writes a new generation, then the pointer.
After writing the pointer it lists again. If the listing changed, another
rebuild may have written an older pointer over this one, so it rebuilds.
Every instance also checks, when it starts and on each `reconcile` tick,
that the pointer names what the bucket holds. That covers an instance that
died between a package and its index.

A replaced generation is deleted once a newer one is older than `grace`.
A client that read InRelease or repomd.xml still finds the files they name
until then. pacman reads `<repo>.db` and then `<repo>.db.sig` in two
requests. If a rebuild lands between them, that sync fails on the
signature, and the next one succeeds. Routing one client to one instance
narrows that window to the second for which an instance trusts a pointer
it read.

## Develop

    bazel run //:lint
    bazel test //...
    go test -tags containers ./internal/clients/

The container tests need rootless podman. They build real packages with
each format's own tools (dpkg-deb, rpmbuild, makepkg, abuild), upload them
to two instances on one SeaweedFS bucket, and install them with apt, dnf,
pacman and apk, each checking the signature. They also show that each
client refuses another key. `bazel run //:hooks` points git at the
repository's hooks: the lint before each commit, and a one-line
conventional subject.
