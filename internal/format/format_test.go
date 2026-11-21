package format

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
	"github.com/sideways-works/piperepo/internal/testpkg"
)

var seed = []byte("the seed of a test's store, long enough")

func signer(t *testing.T, name string) Signer {
	t.Helper()

	if OpenPGP(name) {
		k, err := sign.OpenPGPFromSeed(seed, "test")
		if err != nil {
			t.Fatal(err)
		}

		return Signer{PGP: k}
	}

	k, err := sign.RSAFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}

	return Signer{RSA: k}
}

func parse(t *testing.T, f Format, file string, data []byte) *meta.Package {
	t.Helper()

	if err := f.Accept(file); err != nil {
		t.Fatal(err)
	}

	p, err := f.Parse(file, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(data)
	p.Size, p.SHA256, p.MD5, p.SHA1 = int64(len(data)), string(hexOf(sum[:])), "md5", "sha1"

	return p
}

func hexOf(b []byte) []byte {
	const digits = "0123456789abcdef"

	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}

	return out
}

func keyring(t *testing.T, s Signer) openpgp.EntityList {
	t.Helper()

	pub, err := s.PGP.PublicArmored()
	if err != nil {
		t.Fatal(err)
	}

	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pub))
	if err != nil {
		t.Fatal(err)
	}

	return ring
}

func TestADebIsReadAndListedInASignedRelease(t *testing.T) {
	s := signer(t, "apt")

	f, err := New("apt", Options{}, s)
	if err != nil {
		t.Fatal(err)
	}

	p := parse(t, f, "demo_1.0_all.deb", testpkg.Deb("demo", "1.0", "all"))

	if p.Name != "demo" || p.Version != "1.0" || p.Arch != "all" || p.License != "MIT" || p.URL != "https://example.org/demo" {
		t.Fatalf("read %+v", p)
	}

	if !slices.Equal(p.Depends, []string{"libc6 (>= 2.36)", "zlib1g"}) || p.Description != "made by a test" {
		t.Fatalf("depends %q, description %q", p.Depends, p.Description)
	}

	if !slices.ContainsFunc(p.Files, func(f meta.File) bool { return f.Path == "/usr/share/demo/who" && f.Size > 0 }) {
		t.Fatalf("files %+v", p.Files)
	}

	if got := f.Paths(p); !slices.Equal(got, []string{"pool/main/d/demo/demo_1.0_all.deb"}) {
		t.Fatalf("served at %v", got)
	}

	files, err := f.Index([]*meta.Package{p}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	for _, a := range []string{"amd64", "arm64"} {
		pk := string(files["dists/stable/main/binary-"+a+"/Packages"])
		if !strings.Contains(pk, "Package: demo\n") || !strings.Contains(pk, "Filename: pool/main/d/demo/demo_1.0_all.deb\n") || !strings.Contains(pk, " a longer line\n") {
			t.Fatalf("%s's Packages: %s", a, pk)
		}
	}

	release := files["dists/stable/Release"]
	if !strings.Contains(string(release), "Acquire-By-Hash: yes") {
		t.Fatalf("Release: %s", release)
	}

	block, _ := clearsign.Decode(files["dists/stable/InRelease"])
	if block == nil || !bytes.Equal(block.Plaintext, release) {
		t.Fatal("InRelease doesn't hold Release")
	}

	if _, err := block.VerifySignature(keyring(t, s), nil); err != nil {
		t.Fatal(err)
	}

	hashed := 0

	for path := range files {
		if strings.Contains(path, "/by-hash/SHA256/") {
			hashed++

			if !f.Immutable(path) {
				t.Fatalf("%s isn't immutable", path)
			}
		}
	}

	if hashed != 4 {
		t.Fatalf("%d by-hash files", hashed)
	}
}

func TestAnArchPackageIsListedInSignedDatabases(t *testing.T) {
	s := signer(t, "arch")

	f, err := New("arch", Options{}, s)
	if err != nil {
		t.Fatal(err)
	}

	file := "demo-1.0-1-any.pkg.tar.zst"
	p := parse(t, f, file, testpkg.Arch("demo", "1.0-1", "any"))

	if p.Name != "demo" || p.Version != "1.0-1" || p.Arch != "any" || p.License != "MIT" || !slices.Equal(p.Depends, []string{"glibc"}) {
		t.Fatalf("read %+v", p)
	}

	if slices.ContainsFunc(p.Files, func(f meta.File) bool { return strings.HasPrefix(f.Path, "/.") }) {
		t.Fatalf("files %+v", p.Files)
	}

	if got := f.Paths(p); !slices.Equal(got, []string{"x86_64/" + file, "aarch64/" + file}) {
		t.Fatalf("served at %v", got)
	}

	files, err := f.Index([]*meta.Package{p}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	db := files["x86_64/piperun.db"]

	if _, err := openpgp.CheckDetachedSignature(keyring(t, s), bytes.NewReader(db), bytes.NewReader(files["x86_64/piperun.db.sig"]), nil); err != nil {
		t.Fatal(err)
	}

	entries := tarFiles(t, db)
	if d := entries["demo-1.0-1/desc"]; !strings.Contains(d, "%FILENAME%\n"+file+"\n") || !strings.Contains(d, "%DEPENDS%\nglibc\n") {
		t.Fatalf("desc: %s", d)
	}

	if fl := tarFiles(t, files["aarch64/piperun.files.tar.gz"])["demo-1.0-1/files"]; !strings.Contains(fl, "usr/share/demo/who\n") || !strings.Contains(fl, "usr/share/\n") {
		t.Fatalf("files: %s", fl)
	}
}

func tarFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()

	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]string{}

	err = walkTar(z, func(h *tar.Header, body io.Reader) error {
		b, err := io.ReadAll(body)
		out[h.Name] = string(b)

		return err //nolint:wrapcheck // a test's read
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestAnAPKIsListedInASignedIndex(t *testing.T) {
	s := signer(t, "apk")

	f, err := New("apk", Options{}, s)
	if err != nil {
		t.Fatal(err)
	}

	data := testpkg.APK("demo", "1.0-r0", "noarch")
	p := parse(t, f, "uploaded.apk", data)

	if p.Name != "demo" || p.Version != "1.0-r0" || p.Arch != "noarch" || !strings.HasPrefix(p.APKHash, "Q1") || len(p.Files) == 0 {
		t.Fatalf("read %+v", p)
	}

	if got := f.Paths(p); !slices.Equal(got, []string{"x86_64/demo-1.0-r0.apk", "aarch64/demo-1.0-r0.apk", "noarch/demo-1.0-r0.apk"}) {
		t.Fatalf("served at %v", got)
	}

	files, err := f.Index([]*meta.Package{p}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	idx := files["x86_64/APKINDEX.tar.gz"]

	// Two gzip members: the signature, then the index it signs.
	br := bytes.NewReader(idx)

	z, err := gzip.NewReader(br)
	if err != nil {
		t.Fatal(err)
	}

	z.Multistream(false)

	var sig []byte

	err = walkTar(z, func(h *tar.Header, body io.Reader) error {
		if h.Name != ".SIGN.RSA256.piperun.rsa.pub" {
			t.Fatalf("signature named %s", h.Name)
		}

		sig, err = io.ReadAll(body)

		return err //nolint:wrapcheck // a test's read
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.Copy(io.Discard, io.LimitReader(z, 1<<20)); err != nil {
		t.Fatal(err)
	}

	signed := idx[len(idx)-br.Len():]

	sum := sha256.Sum256(signed)
	if err := rsa.VerifyPKCS1v15(&s.RSA.Key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatal(err)
	}

	index := tarFiles(t, signed)["APKINDEX"]
	if !strings.Contains(index, "C:"+p.APKHash+"\nP:demo\nV:1.0-r0\nA:noarch\n") || !strings.Contains(index, "D:musl\n") {
		t.Fatalf("APKINDEX: %s", index)
	}
}

func TestAnRPMIsListedInSignedRepodata(t *testing.T) {
	s := signer(t, "rpm")

	f, err := New("rpm", Options{}, s)
	if err != nil {
		t.Fatal(err)
	}

	p := parse(t, f, "demo-1.0-1.noarch.rpm", testpkg.RPM("demo", "1.0", "1", "noarch"))

	if p.Name != "demo" || p.Version != "1.0-1" || p.Arch != "noarch" || p.License != "MIT" || p.Description != "made by a test" {
		t.Fatalf("read %+v", p)
	}

	if !slices.Equal(p.Depends, []string{"/bin/sh", "glibc"}) || p.RPM.HeaderStart == 0 || p.RPM.HeaderEnd <= p.RPM.HeaderStart {
		t.Fatalf("depends %v, header %d-%d", p.Depends, p.RPM.HeaderStart, p.RPM.HeaderEnd)
	}

	if !slices.ContainsFunc(p.Files, func(f meta.File) bool { return f.Path == "/usr/share/demo/who" && f.Size == 12 }) ||
		!slices.ContainsFunc(p.Files, func(f meta.File) bool { return f.Path == "/usr/share/demo" && f.Dir }) {
		t.Fatalf("files %+v", p.Files)
	}

	if g := p.RPM.Requires[len(p.RPM.Requires)-1]; g.Name != "glibc" || g.Flags != "GE" || g.Ver != "2.36" {
		t.Fatalf("requires %+v", p.RPM.Requires)
	}

	files, err := f.Index([]*meta.Package{p}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	md := files["repodata/repomd.xml"]

	if _, err := openpgp.CheckArmoredDetachedSignature(keyring(t, s), bytes.NewReader(md), bytes.NewReader(files["repodata/repomd.xml.asc"]), nil); err != nil {
		t.Fatal(err)
	}

	for path, data := range files {
		if !strings.HasSuffix(path, "-primary.xml.gz") {
			continue
		}

		if !strings.Contains(string(md), path) || !f.Immutable(path) {
			t.Fatalf("repomd.xml doesn't name %s", path)
		}

		z, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}

		primary, err := io.ReadAll(z)
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(primary), `<location href="packages/demo-1.0-1.noarch.rpm"/>`) || strings.Contains(string(primary), "rpmlib(") {
			t.Fatalf("primary: %s", primary)
		}

		return
	}

	t.Fatal("no primary")
}

func TestPackagesOfAnotherFormatAreRefused(t *testing.T) {
	cases := map[string][]byte{"apt": testpkg.APK("x", "1-r0", "noarch"), "arch": testpkg.Deb("x", "1", "all"), "apk": []byte("not a package"), "rpm": testpkg.Deb("x", "1", "all")}
	names := map[string]string{"apt": "x.deb", "arch": "x-1-1-any.pkg.tar.zst", "apk": "x.apk", "rpm": "x-1-1.noarch.rpm"}

	for name, data := range cases {
		f, err := New(name, Options{}, signer(t, name))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := f.Parse(names[name], bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrFormat) {
			t.Errorf("%s took another format's package: %v", name, err)
		}

		if err := f.Accept("x.exe"); err == nil {
			t.Errorf("%s takes x.exe", name)
		}
	}
}
