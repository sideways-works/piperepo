//go:build containers

package clients

import (
	"fmt"
	"strings"
	"testing"
)

// archBuild builds a package with makepkg as a user of its own.
const archBuild = `p=%[1]s; v=%[2]s
mkdir -p "/tmp/$p"
printf 'pkgname=%%s\npkgver=%%s\npkgrel=1\npkgdesc="made by a test"\narch=(any)\nlicense=(MIT)\noptions=(!debug !strip)\npackage() { mkdir -p "$pkgdir/usr/share/%%s"; echo "built %%s %%s" > "$pkgdir/usr/share/%%s/who"; }\n' "$p" "$v" "$p" "$p" "$v" "$p" > "/tmp/$p/PKGBUILD"
chown -R builder "/tmp/$p"
su builder -c "cd /tmp/$p && makepkg --nodeps --noconfirm >/dev/null"
cp /tmp/$p/*.pkg.tar.zst /w/
`

// pacmanConf is a configuration of the store alone: its database signed,
// its packages not.
const pacmanConf = `printf "[options]\nArchitecture = auto\nSigLevel = Required DatabaseOptional\n[piperun]\nSigLevel = PackageOptional DatabaseRequired\nServer = http://piperun:%s@%s/\$arch\n" %s > /w/pacman.conf
`

// Two packages, each uploaded to another instance, install with pacman,
// which checks the database's signature with the served key, locally
// signed into its keyring.
func TestPacmanInstallsFromTheSignedRepository(t *testing.T) {
	podman(t)

	s := serve(t, "arch")
	dir := workdir(t)

	inside(t, imageArch, dir, "useradd -m builder\n"+fmt.Sprintf(archBuild, "demo", "1.0.0")+fmt.Sprintf(archBuild, "other", "2.0.0"))
	upload(t, s.urls[0], dir, "demo-1.0.0-1-any.pkg.tar.zst")
	upload(t, s.urls[1], dir, "other-2.0.0-1-any.pkg.tar.zst")
	fetch(t, s.urls[0], "key.asc", dir, "key.asc")
	otherKey(t, dir)

	listed(t, s.urls[1], "1.0.0-1")
	hp := hostport(s.urls[1])
	trust := `pacman-key --init >/dev/null 2>&1
pacman-key --add /w/%[1]s
fpr=$(gpg --with-colons --import-options show-only --import /w/%[1]s 2>/dev/null | awk -F: '/^fpr/ {print $10; exit}')
pacman-key --lsign-key "$fpr"
`

	out := inside(t, imageArch, dir, fmt.Sprintf(trust, "key.asc")+fmt.Sprintf(pacmanConf, token, hp, "")+`
pacman --config /w/pacman.conf -Sy --noconfirm demo other
cat /usr/share/demo/who /usr/share/other/who
`)

	if !strings.Contains(out, "built demo 1.0.0") || !strings.Contains(out, "built other 2.0.0") {
		t.Fatalf("installed:\n%s", out)
	}

	refused := inside(t, imageArch, dir, fmt.Sprintf(trust, "other.asc")+fmt.Sprintf(pacmanConf, token, hp, "")+`
if pacman --config /w/pacman.conf -Sy --noconfirm demo 2>&1; then echo "pacman took another key" >&2; exit 1; fi
`)

	if !strings.Contains(refused, "signature") {
		t.Fatalf("pacman refused, but not for the signature:\n%s", refused)
	}

	t.Logf("with another key pacman says:\n%s", refused)
}
