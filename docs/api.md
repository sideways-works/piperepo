# HTTP API

This page describes every request piperepo answers: uploads, the package
listing, removal and the published tree. Every request but `GET /health`
needs the credential, as the password of basic authentication (any user)
or as `Authorization: Bearer <token>`. Without it the answer is 401.

Errors come back as JSON, `{"error": "<what went wrong>"}`.

## Health

`GET /health` answers `ok`. It needs no credential.

## Upload a package

`PUT /upload/<file>` takes the package as the body:

```sh
curl -fsSu "piperun:$TOKEN" -T demo_1.0.0_all.deb https://repo.example.com/upload/demo_1.0.0_all.deb
```

It answers 201 once an index that lists the package is served, so a client
can install it right after. The answer's headers name what was read from
the package: `Piperepo-Name`, `Piperepo-Version`, `Piperepo-Arch` and
`Piperepo-Size`. The body is the package's entry, as the listing below
gives it.

| Status | When |
|---|---|
| 201 | The package is published. |
| 400 | The file isn't a package of this instance's format, or it can't be read. |
| 409 | The file name is taken, with other bytes, and `overwrite` is off. |
| 413 | The package is larger than `max_size`. |

Uploading the same bytes under the same name again succeeds and changes
nothing.

## List packages

`GET /api/packages` lists every package; `?name=<name>` lists one
package's versions. Each entry is:

```json
{"name": "demo", "version": "1.0.0", "arch": "all", "file": "demo_1.0.0_all.deb",
 "size": 748, "sha256": "...", "description": "a first package", "license": "", "uploaded": "..."}
```

## One version

`GET /api/packages/<name>/<version>` answers one object:

```json
{"name": "demo", "version": "1.0.0", "archs": ["all"], "description": "...", "license": "...",
 "size": 748, "depends": [{"name": "libc6", "range": ">= 2.36"}],
 "files": [{"path": "/usr/share/demo/hello", "size": 20}]}
```

The files are those of the first architecture. A version that isn't there
gets 404.

## Remove a package

`DELETE /api/packages/<file>` removes the package and rebuilds the index.
It answers 204, or 404 when there is no such file.

## The published tree

Any other `GET` (or `HEAD`) is a path of the published tree, which the
clients read; [how it works](architecture.md#what-each-format-publishes)
lists the paths. Index files are sent with `Cache-Control: no-cache`.
Packages and files named by their hash are sent as immutable. Every answer
has an ETag, and a matching `If-None-Match` gets 304.
