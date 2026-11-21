//go:build containers

package clients

import (
	"fmt"
	"strings"
	"testing"
)

// debBuild builds a package with dpkg-deb: who says which build made it.
const debBuild = `p=%[1]s; v=%[2]s
mkdir -p "$p/DEBIAN" "$p/usr/share/$p"
printf 'Package: %%s\nVersion: %%s\nArchitecture: all\nMaintainer: piperepo <piperepo@localhost>\nDescription: made by a test\n' "$p" "$v" > "$p/DEBIAN/control"
echo "built $p $v" > "$p/usr/share/$p/who"
dpkg-deb --root-owner-group --build "$p" "${p}_${v}_all.deb"
`

// Two packages, each uploaded to another instance, install with apt from
// the signed repository, its key named by signed-by alone.
func TestAptInstallsFromTheSignedRepository(t *testing.T) {
	podman(t)

	s := serve(t, "apt")
	dir := workdir(t)

	inside(t, imageDebian, dir, fmt.Sprintf(debBuild, "demo", "1.0.0")+fmt.Sprintf(debBuild, "other", "2.0.0"))
	upload(t, s.urls[0], dir, "demo_1.0.0_all.deb")
	upload(t, s.urls[1], dir, "other_2.0.0_all.deb")
	fetch(t, s.urls[0], "key.asc", dir, "key.asc")

	listed(t, s.urls[1], "1.0.0")
	hp := hostport(s.urls[1])
	out := inside(t, imageDebian, dir, `echo "deb [signed-by=/w/key.asc] http://`+hp+` stable main" > /etc/apt/sources.list.d/piperepo.list
printf 'machine http://`+hp+` login piperun password %s\n' "`+token+`" > /etc/apt/auth.conf.d/piperepo.conf
chmod 644 /etc/apt/auth.conf.d/piperepo.conf
apt-get -o APT::Update::Error-Mode=any update
apt-get install -y demo other
cat /usr/share/demo/who /usr/share/other/who
`)

	if !strings.Contains(out, "built demo 1.0.0") || !strings.Contains(out, "built other 2.0.0") {
		t.Fatalf("installed:\n%s", out)
	}

	// Another key: apt refuses the repository.
	otherKey(t, dir)
	refused := inside(t, imageDebian, dir, `echo "deb [signed-by=/w/other.asc] http://`+hp+` stable main" > /etc/apt/sources.list.d/piperepo.list
printf 'machine http://`+hp+` login piperun password %s\n' "`+token+`" > /etc/apt/auth.conf.d/piperepo.conf
chmod 644 /etc/apt/auth.conf.d/piperepo.conf
if apt-get -o APT::Update::Error-Mode=any update 2>&1; then echo "apt took another key" >&2; exit 1; fi
`)

	if !strings.Contains(refused, "NO_PUBKEY") && !strings.Contains(refused, "not signed") {
		t.Fatalf("apt refused, but not for the signature:\n%s", refused)
	}

	t.Logf("with another key apt says:\n%s", refused)
}
