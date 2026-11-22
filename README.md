<img src="docs/brand/banner.svg" alt="piperepo, by Sideways Works" width="400">

# piperepo

piperepo serves a signed package repository for apt, pacman, apk or dnf
straight from an S3 bucket.

You upload a package over HTTP. piperepo reads it, rebuilds the index,
signs it and publishes it, and the distribution's own client installs from
it as from any other repository. Each instance serves one format. It keeps
no database and no state on disk, so you can run as many instances on one
bucket as you like, and none of them waits for another.

```mermaid
flowchart LR
  ci[build or person] -- PUT /upload --> a[piperepo]
  ci -- PUT /upload --> b[piperepo]
  a <-- packages, indexes, pointer --> s3[(S3 bucket)]
  b <-- packages, indexes, pointer --> s3
  apt[apt, pacman, apk, dnf] -- GET signed tree --> a
  apt -- GET signed tree --> b
```

## What it does

- It serves four formats. `apt` is a Debian repository (`dists/`, `pool/`,
  by-hash indexes), `arch` a pacman database with its files database,
  `apk` an Alpine `APKINDEX` per architecture, and `rpm` a dnf repository
  with `repodata/`. A package built for every architecture (`all`, `any`,
  `noarch`) is listed under each one.
- It signs every index it builds: apt's `InRelease` and `Release.gpg`,
  pacman's database `.sig`, dnf's `repomd.xml.asc` and apk's
  `.SIGN.RSA256`. The public key is served beside the tree. Each client
  checks the signature and refuses a repository signed by another key.
- It needs no key file. Give every instance the same seed and each makes
  the same signing key from it. A key file works too.
- It stores everything in S3: packages, their metadata and every built
  index. Any instance can rebuild the whole index from the packages alone.
- Its instances don't coordinate. Each rebuild writes a new index
  generation and then one pointer to it. An instance that sees the bucket
  change under it rebuilds again, and each one checks on a timer that the
  index lists what the bucket holds, so a crash between a package and its
  index heals itself. There are no locks.
- It is private. Every request but `GET /health` needs the credential, as
  a basic authentication password or a bearer token.

## Quick start

This runs piperepo for apt against a throwaway S3 bucket, uploads a small
package and installs it with apt in a Debian container. You need Go and
podman (or docker).

Start an S3 server and make a bucket. Any S3 works; SeaweedFS is quick to
try and takes any key:

```sh
podman run -d --name s3 -p 8333:8333 docker.io/chrislusf/seaweedfs server -s3
until curl -fsX PUT http://127.0.0.1:8333/debian; do sleep 1; done
```

Build piperepo, make a credential and a signing seed, and start it:

```sh
git clone https://github.com/sideways-works/piperepo && cd piperepo
CGO_ENABLED=0 go build ./cmd/piperepo
head -c 32 /dev/urandom | base64 > token
head -c 32 /dev/urandom | base64 > seed
cat > piperepo.yaml <<'EOF'
listen: 127.0.0.1:8080
s3: {endpoint: http://127.0.0.1:8333, bucket: debian, access_key: any, secret_key: any, path_style: true}
token_file: token
seed_file: seed
EOF
./piperepo serve --format apt --config piperepo.yaml
```

In another shell in the same directory, build a package and upload it:

```sh
podman run --rm -v "$PWD:/w" -w /w docker.io/library/debian:trixie sh -c '
  mkdir -p demo/DEBIAN demo/usr/share/demo
  printf "Package: demo\nVersion: 1.0.0\nArchitecture: all\nMaintainer: you <you@example.com>\nDescription: a first package\n" > demo/DEBIAN/control
  echo "hello from piperepo" > demo/usr/share/demo/hello
  dpkg-deb --root-owner-group --build demo demo_1.0.0_all.deb'
curl -fsSu "piperun:$(cat token)" -T demo_1.0.0_all.deb http://127.0.0.1:8080/upload/demo_1.0.0_all.deb
```

Then install it with apt, which checks the index's signature:

```sh
curl -fsSu "piperun:$(cat token)" -o key.asc http://127.0.0.1:8080/key.asc
podman run --rm --network host -v "$PWD:/w:ro" -e TOKEN="$(cat token)" docker.io/library/debian:trixie sh -c '
  echo "deb [signed-by=/w/key.asc] http://127.0.0.1:8080 stable main" > /etc/apt/sources.list.d/demo.list
  printf "machine http://127.0.0.1:8080 login piperun password %s\n" "$TOKEN" > /etc/apt/auth.conf.d/demo.conf
  apt-get update && apt-get install -y demo && cat /usr/share/demo/hello'
```

When you're done, stop piperepo and remove the S3 container with
`podman rm -f s3`.

## Run it

piperepo is one static binary with one command:

```sh
piperepo serve --format apt|arch|apk|rpm --config piperepo.yaml
```

It needs no libraries and runs in any small base image, such as Alpine.
Mount the configuration, the credential and the seed (or key) as files.
The configuration names the bucket, the credential file and the signing
key:

```yaml
listen: 0.0.0.0:8080
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: packages
  access_key: ...
  secret_key: ...
token_file: /run/secrets/token
seed_file: /run/secrets/seed # or key_file
```

Everything else has a default: apt serves suite `stable`, component
`main`, for `amd64` and `arm64`; pacman's database is `piperun`, for
`x86_64` and `aarch64`; apk's key is named `piperun.rsa.pub`. Every key is
in [the configuration reference](docs/configuration.md).

To run more instances, start them with the same bucket, credential and
seed, and put them behind any load balancer. They need nothing else to
share.

## Install from it

Each client sends the credential as its password, with any user name. Only
the indexes are signed, as is usual for apt, pacman, apk and dnf
repositories: each package is checked against the hash the signed index
lists. Replace `repo.example.com` and `TOKEN` with your instance and
credential.

### apt

On Debian or Ubuntu, with the key from `/key.asc`:

```sh
curl -fsSu "piperun:TOKEN" -o /etc/apt/keyrings/piperepo.asc https://repo.example.com/key.asc
echo "deb [signed-by=/etc/apt/keyrings/piperepo.asc] https://repo.example.com stable main" > /etc/apt/sources.list.d/piperepo.list
printf 'machine https://repo.example.com login piperun password %s\n' TOKEN > /etc/apt/auth.conf.d/piperepo.conf
apt-get update && apt-get install demo
```

### pacman

On Arch Linux, with the key from `/key.asc` signed into pacman's
keyring:

```sh
curl -fsSu "piperun:TOKEN" -o key.asc https://repo.example.com/key.asc
pacman-key --add key.asc
pacman-key --lsign-key "$(gpg --with-colons --import-options show-only --import key.asc | awk -F: '/^fpr/ {print $10; exit}')"
cat >> /etc/pacman.conf <<'EOF'
[piperun]
SigLevel = PackageOptional DatabaseRequired
Server = https://piperun:TOKEN@repo.example.com/$arch
EOF
pacman -Sy demo
```

### apk

On Alpine, with the key from `/keys/piperun.rsa.pub`:

```sh
curl -fsSu "piperun:TOKEN" -o /etc/apk/keys/piperun.rsa.pub https://repo.example.com/keys/piperun.rsa.pub
echo "https://piperun:TOKEN@repo.example.com" >> /etc/apk/repositories
apk update && apk add demo
```

### dnf

On Fedora or RHEL, with the key from `/key.asc`:

```sh
curl -fsSu "piperun:TOKEN" -o /etc/pki/rpm-gpg/piperepo.asc https://repo.example.com/key.asc
cat > /etc/yum.repos.d/piperepo.repo <<'EOF'
[piperepo]
name=piperepo
baseurl=https://repo.example.com/
username=piperun
password=TOKEN
gpgcheck=0
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/piperepo.asc
EOF
dnf install demo
```

## piperun

[piperun](https://github.com/sideways-works/piperun), a CI system, runs
piperepo as its `debian`, `rpm`, `arch` and `alpine` package stores. Each
store's plugin builds piperepo from a pinned commit and runs it in Alpine
on every node its placement names, with one bucket and one signing key per
tenant. piperun's gateway holds the credential and lets only trusted
builds upload. piperepo itself knows nothing of piperun.

## Docs

- [Configuration](docs/configuration.md): every key and its default.
- [HTTP API](docs/api.md): uploads, the package listing, removal and the
  published tree.
- [How it works](docs/architecture.md): what each format publishes, what
  the bucket holds, and how instances stay consistent without locks.

## Building and testing

```sh
bazel run //:lint      # go vet and golangci-lint
bazel test //...
go test -tags containers ./internal/clients/
```

The container tests need rootless podman. They build real packages with
each format's own tools (dpkg-deb, rpmbuild, makepkg, abuild), upload them
to two instances on one SeaweedFS bucket, and install them with apt, dnf,
pacman and apk, each checking the signature. They also check that each
client refuses a repository signed with another key.

`bazel run //:hooks` points git at the repository's hooks: the lint before
each commit, and one conventional subject line of at most 72 characters
for each commit message. Pull requests are welcome; please run the lint
and the tests first.

## License

piperepo is licensed under the [GNU Affero General Public License, version
3](LICENSE) or (at your option) any later version.
