//go:build containers

package clients

import (
	"fmt"
	"strings"
	"testing"
)

// rpmBuild builds a package with rpmbuild: who says which build made it.
const rpmBuild = `p=%[1]s; v=%[2]s
printf 'Name: %%s\nVersion: %%s\nRelease: 1\nSummary: made by a test\nLicense: MIT\nBuildArch: noarch\n%%%%description\nmade by a test\n%%%%install\nmkdir -p %%%%{buildroot}/usr/share/%%s\necho "built %%s %%s" > %%%%{buildroot}/usr/share/%%s/who\n%%%%files\n/usr/share/%%s/who\n' "$p" "$v" "$p" "$p" "$v" "$p" "$p" > "$p.spec"
rpmbuild -bb --define "_topdir /tmp/rpm" "$p.spec" >/dev/null
cp /tmp/rpm/RPMS/noarch/*.rpm .
rm -rf /tmp/rpm
`

// dnfRepo names the store with repo_gpgcheck, its key the file given.
const dnfRepo = `printf "[piperepo]\nname=piperepo\nbaseurl=http://%s/\nusername=piperun\npassword=%s\nenabled=1\ngpgcheck=0\nrepo_gpgcheck=1\ngpgkey=file://%s\n" > /etc/yum.repos.d/piperepo.repo
`

// Two packages, each uploaded to another instance, install with dnf, which
// checks repomd.xml's signature with the served key.
func TestDnfInstallsFromTheSignedRepository(t *testing.T) {
	podman(t)

	s := serve(t, "rpm")
	dir := workdir(t)

	inside(t, imageFedora, dir, "dnf -q -y install rpm-build >/dev/null\n"+fmt.Sprintf(rpmBuild, "demo", "1.0.0")+fmt.Sprintf(rpmBuild, "other", "2.0.0"))
	upload(t, s.urls[0], dir, "demo-1.0.0-1.noarch.rpm")
	upload(t, s.urls[1], dir, "other-2.0.0-1.noarch.rpm")
	fetch(t, s.urls[0], "key.asc", dir, "key.asc")

	listed(t, s.urls[1], "1.0.0-1")
	hp := hostport(s.urls[1])
	out := inside(t, imageFedora, dir, fmt.Sprintf(dnfRepo, hp, token, "/w/key.asc")+`
dnf -y --disablerepo='*' --enablerepo=piperepo install demo other
cat /usr/share/demo/who /usr/share/other/who
`)

	if !strings.Contains(out, "built demo 1.0.0") || !strings.Contains(out, "built other 2.0.0") {
		t.Fatalf("installed:\n%s", out)
	}

	// Another key: dnf refuses the repository.
	otherKey(t, dir)
	refused := inside(t, imageFedora, dir, fmt.Sprintf(dnfRepo, hp, token, "/w/other.asc")+`
if dnf -y --disablerepo='*' --enablerepo=piperepo install demo 2>&1; then echo "dnf took another key" >&2; exit 1; fi
`)

	if !strings.Contains(strings.ToLower(refused), "signature") {
		t.Fatalf("dnf refused, but not for the signature:\n%s", refused)
	}

	t.Logf("with another key dnf says:\n%s", refused)
}
