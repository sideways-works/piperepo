//go:build containers

package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sideways-works/piperepo/internal/sign"
)

// apkBuild builds a package with abuild, signed by a key of its own that
// no client trusts: the index's signature is what apk checks.
const apkBuild = `p=%[1]s; v=%[2]s
mkdir -p "/tmp/$p" && cd "/tmp/$p"
printf 'pkgname=%%s\npkgver=%%s\npkgrel=0\npkgdesc="made by a test"\nurl="https://example.org"\narch="noarch"\nlicense="MIT"\noptions="!check"\npackage() { mkdir -p "$pkgdir/usr/share/%%s"; echo "built %%s %%s" > "$pkgdir/usr/share/%%s/who"; }\n' "$p" "$v" "$p" "$p" "$v" "$p" > APKBUILD
abuild -F rootpkg >/dev/null 2>&1
cp /root/packages/tmp/x86_64/$p-$v-r0.apk /w/
cd /w
`

// apkRepo trusts the key file given and names the store alone.
const apkRepo = `rm -f /etc/apk/keys/*
cp /w/%s /etc/apk/keys/piperun.rsa.pub
echo "http://piperun:%s@%s" > /etc/apk/repositories
`

// Two packages, each uploaded to another instance, install with apk, which
// checks APKINDEX's signature with the served key alone.
func TestApkInstallsFromTheSignedRepository(t *testing.T) {
	podman(t)

	s := serve(t, "apk", 2)
	dir := workdir(t)

	out := inside(t, imageAlpine, dir, "apk --version\napk add -q abuild >/dev/null\nabuild-keygen -a -n >/dev/null 2>&1\n"+fmt.Sprintf(apkBuild, "demo", "1.0.0")+fmt.Sprintf(apkBuild, "other", "2.0.0"))
	t.Logf("built with %s", strings.TrimSpace(out))
	upload(t, s.urls[0], dir, "demo-1.0.0-r0.apk")
	upload(t, s.urls[1], dir, "other-2.0.0-r0.apk")
	fetch(t, s.urls[0], "keys/piperun.rsa.pub", dir, "piperun.rsa.pub")

	hp := hostport(s.urls[1])

	out = inside(t, imageAlpine, dir, fmt.Sprintf(apkRepo, "piperun.rsa.pub", token, hp)+`apk update
apk add demo other
cat /usr/share/demo/who /usr/share/other/who
`)

	if !strings.Contains(out, "built demo 1.0.0") || !strings.Contains(out, "built other 2.0.0") {
		t.Fatalf("installed:\n%s", out)
	}

	k, err := sign.RSAFromSeed([]byte("another store's seed, not this one's"))
	if err != nil {
		t.Fatal(err)
	}

	pub, err := k.PublicPEM()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "other.rsa.pub"), pub, 0o644); err != nil { //nolint:gosec // read by the client container
		t.Fatal(err)
	}

	refused := inside(t, imageAlpine, dir, fmt.Sprintf(apkRepo, "other.rsa.pub", token, hp)+`apk update 2>&1 || true
if apk add demo 2>&1; then echo "apk took another key" >&2; exit 1; fi
`)

	if !strings.Contains(refused, "BAD signature") && !strings.Contains(strings.ToUpper(refused), "UNTRUSTED") {
		t.Fatalf("apk refused, but not for the signature:\n%s", refused)
	}

	t.Logf("with another key apk says:\n%s", refused)
}
