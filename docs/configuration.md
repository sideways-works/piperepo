# Configuration

This page lists every key of piperepo's configuration file, with its
default. piperepo refuses a file with a key it doesn't know, and says which
required key is missing.

```sh
piperepo serve --format apt|arch|apk|rpm --config piperepo.yaml
```

## A full example

```yaml
listen: 127.0.0.1:8080
s3:
  endpoint: http://127.0.0.1:8333
  region: us-east-1
  bucket: debian
  access_key: ...
  secret_key: ...
  path_style: true
token_file: /run/secrets/token
seed_file: /run/secrets/seed
overwrite: false
max_size: 2147483648
reconcile: 10s
grace: 10m
temp_dir: /tmp
origin: piperepo
apt: {suite: stable, component: main, architectures: [amd64, arm64]}
arch: {repo: piperun, architectures: [x86_64, aarch64]}
apk: {key_name: piperun.rsa.pub, architectures: [x86_64, aarch64]}
```

Only the keys of the format you serve are read; the others may stay.

## Keys

| Key | Default | What it does |
|---|---|---|
| `listen` | `127.0.0.1:8080` | The address the HTTP server listens on. |
| `s3.endpoint` | AWS S3 | The S3 endpoint. Leave it out for AWS. |
| `s3.region` | `us-east-1` | The bucket's region. |
| `s3.bucket` | required | The bucket the packages and indexes go to. |
| `s3.access_key`, `s3.secret_key` | none | The S3 credentials. |
| `s3.path_style` | `false` | Path-style URLs, which most S3 servers other than AWS need. |
| `token_file` | required | A file holding the credential every request but `GET /health` needs. |
| `seed_file` | | A file holding a seed the signing key is made from. Set this or `key_file`. |
| `key_file` | | A file holding the signing key. Set this or `seed_file`. |
| `overwrite` | `false` | Take a file name again with other bytes. When false, that upload gets 409. |
| `max_size` | `2147483648` | The largest package taken, in bytes. `0` takes any size. |
| `reconcile` | `10s` | How often an instance checks that the index lists what the bucket holds. |
| `grace` | `10m` | How long a replaced index generation stays readable. |
| `temp_dir` | the system's | Where uploads are spooled while they are read. |
| `origin` | `piperepo` | The repository's name in its indexes and in a seeded key's user ID. |
| `apt.suite` | `stable` | apt's suite, as in `deb <url> stable main`. |
| `apt.component` | `main` | apt's component. |
| `apt.architectures` | `[amd64, arm64]` | The architectures apt's index lists. |
| `arch.repo` | `piperun` | pacman's database name, the `[section]` in `pacman.conf`. |
| `arch.architectures` | `[x86_64, aarch64]` | The architectures pacman's databases are built for. |
| `apk.key_name` | `piperun.rsa.pub` | The key's file name, which apk looks for in `/etc/apk/keys`. |
| `apk.architectures` | `[x86_64, aarch64]` | The architectures an `APKINDEX` is built for. |

## The signing key

piperepo refuses to start without a signing key, since it could sign
nothing.

`key_file` holds an armored OpenPGP secret key for `apt`, `arch` and
`rpm`, or a PEM RSA key for `apk`.

With `seed_file`, each instance makes the same key from the seed: an
Ed25519 OpenPGP key with a fixed creation time, or a 4096-bit RSA key for
`apk`. Every instance of one store signs alike, and no key is stored
anywhere. Keep the seed as secret as you would keep a key.

## The credential

`token_file` holds one line. A client sends it as the password of basic
authentication, with any user name, or as `Authorization: Bearer <token>`.
A trailing newline is ignored.
