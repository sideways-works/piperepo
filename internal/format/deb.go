package format

import (
	"archive/tar"
	"bytes"
	"crypto/md5" //nolint:gosec // apt's Packages lists MD5sum
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
)

// deb is an apt repository: one suite, one component.
type deb struct {
	o   Options
	pgp *sign.OpenPGP
}

func newDeb(o Options, pgp *sign.OpenPGP) *deb {
	if o.Suite == "" {
		o.Suite = "stable"
	}

	if o.Component == "" {
		o.Component = "main"
	}

	o.Archs = archsOr(o.Archs, []string{"amd64", "arm64"})

	return &deb{o: o, pgp: pgp}
}

func (d *deb) Name() string { return Apt }

func (d *deb) Accept(file string) error {
	if !strings.HasSuffix(file, ".deb") {
		return fmt.Errorf("%w: %s: an apt store takes .deb files", ErrFormat, file)
	}

	return nil
}

// arMember is one member of an ar archive.
type arMember struct {
	name string
	body *io.SectionReader
}

// arMembers are a .deb's members, in order.
func arMembers(r io.ReaderAt, size int64) ([]arMember, error) {
	magic := make([]byte, 8)
	if _, err := r.ReadAt(magic, 0); err != nil || string(magic) != "!<arch>\n" {
		return nil, fmt.Errorf("%w: not an ar archive", ErrFormat)
	}

	var out []arMember

	for off := int64(8); off+60 <= size; {
		h := make([]byte, 60)
		if _, err := r.ReadAt(h, off); err != nil {
			return nil, fmt.Errorf("%w: ar: %w", ErrFormat, err)
		}

		n, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil || n < 0 || off+60+n > size {
			return nil, fmt.Errorf("%w: ar: a member's size", ErrFormat)
		}

		name := strings.TrimSuffix(strings.TrimSpace(string(h[:16])), "/")
		out = append(out, arMember{name: name, body: io.NewSectionReader(r, off+60, n)})
		off += 60 + n + n%2
	}

	return out, nil
}

func (d *deb) Parse(file string, r io.ReaderAt, size int64) (*meta.Package, error) {
	members, err := arMembers(r, size)
	if err != nil {
		return nil, err
	}

	if len(members) < 3 || members[0].name != "debian-binary" {
		return nil, fmt.Errorf("%w: %s: not a Debian package", ErrFormat, file)
	}

	p := &meta.Package{File: file}

	var control, data bool

	for _, m := range members[1:] {
		switch {
		case strings.HasPrefix(m.name, "control.tar"):
			control = true

			if err := readDebControl(p, m); err != nil {
				return nil, err
			}
		case strings.HasPrefix(m.name, "data.tar"):
			data = true

			if err := readDebData(p, m); err != nil {
				return nil, err
			}
		}
	}

	if !control || !data {
		return nil, fmt.Errorf("%w: %s: a Debian package has a control and a data member", ErrFormat, file)
	}

	p.Name, p.Version, p.Arch = p.Get("Package"), p.Get("Version"), p.Get("Architecture")
	p.Description = oneLine(p.Get("Description"))
	p.URL = p.Get("Homepage")
	p.Depends = splitDeps(p.Get("Depends"))

	if kb, err := strconv.ParseInt(p.Get("Installed-Size"), 10, 64); err == nil {
		p.Installed = kb * 1024
	}

	return p, p.Check()
}

func readDebControl(p *meta.Package, m arMember) error {
	r, err := decompress(m.name, m.body)
	if err != nil {
		return err
	}

	return walkTar(r, func(h *tar.Header, body io.Reader) error {
		if cleanPath(h.Name) != "control" {
			return nil
		}

		b, err := io.ReadAll(io.LimitReader(body, 1<<20))
		if err != nil {
			return fmt.Errorf("%w: control: %w", ErrFormat, err)
		}

		p.Fields = parseControl(string(b))

		return nil
	})
}

func readDebData(p *meta.Package, m arMember) error {
	r, err := decompress(m.name, m.body)
	if err != nil {
		return err
	}

	return walkTar(r, func(h *tar.Header, body io.Reader) error {
		f, ok := fileOf(h)
		if !ok {
			return nil
		}

		p.Files = append(p.Files, f)

		// A machine-readable copyright file names the license.
		if p.License == "" && strings.HasPrefix(f.Path, "/usr/share/doc/") && strings.HasSuffix(f.Path, "/copyright") && h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(io.LimitReader(body, 64<<10)) //nolint:errcheck // a license is optional
			for line := range strings.SplitSeq(string(b), "\n") {
				if v, ok := strings.CutPrefix(line, "License:"); ok && strings.TrimSpace(v) != "" {
					p.License = strings.TrimSpace(v)

					break
				}
			}
		}

		return nil
	})
}

// parseControl reads a control paragraph's fields in order, each
// continuation line kept with its field.
func parseControl(s string) []meta.Field {
	var out []meta.Field

	for line := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			break
		}

		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1].Value += "\n" + line

			continue
		}

		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		out = append(out, meta.Field{Key: k, Value: strings.TrimSpace(v)})
	}

	return out
}

func splitDeps(s string) []string {
	var out []string

	for d := range strings.SplitSeq(s, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}

	return out
}

// poolDir is a package's directory in the pool: by its first letter, or
// its first four for a library.
func (d *deb) poolDir(name string) string {
	prefix := name[:1]
	if strings.HasPrefix(name, "lib") && len(name) > 3 {
		prefix = name[:4]
	}

	return path.Join("pool", d.o.Component, prefix, name)
}

func (d *deb) Paths(p *meta.Package) []string {
	return []string{path.Join(d.poolDir(p.Name), p.File)}
}

func (d *deb) Keys() (map[string][]byte, error) {
	k, err := d.pgp.PublicArmored()
	if err != nil {
		return nil, err
	}

	return map[string][]byte{keyFile: k}, nil
}

func (d *deb) Immutable(p string) bool { return strings.Contains(p, "/by-hash/") }

// debSkip are control fields apt's Packages writes itself.
var debSkip = []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256"}

// debFile is an index file Release names, by its path under the suite.
type debFile struct {
	name string
	data []byte
}

func (d *deb) Index(pkgs []*meta.Package, at time.Time) (map[string][]byte, error) {
	out := map[string][]byte{}
	dist := path.Join("dists", d.o.Suite)
	sums := d.packagesFiles(pkgs, dist, out)
	release := d.release(sums, at)

	in, err := d.pgp.ClearSign(release)
	if err != nil {
		return nil, err
	}

	det, err := d.pgp.Detach(release, true)
	if err != nil {
		return nil, err
	}

	out[path.Join(dist, "Release")] = release
	out[path.Join(dist, "InRelease")] = in
	out[path.Join(dist, "Release.gpg")] = det

	return out, nil
}

// packagesFiles writes each architecture's Packages files into out, by
// their names and by their hashes, and answers them for Release.
func (d *deb) packagesFiles(pkgs []*meta.Package, dist string, out map[string][]byte) []debFile {
	byArch := map[string]*bytes.Buffer{}

	for _, a := range d.o.Archs {
		byArch[a] = &bytes.Buffer{}
	}

	for _, p := range sortPackages(pkgs) {
		for _, a := range archsOf(p.Arch, d.o.Archs, "all") {
			b, ok := byArch[a]
			if !ok {
				continue
			}

			for _, f := range p.Fields {
				if !slices.Contains(debSkip, f.Key) {
					fmt.Fprintf(b, "%s: %s\n", f.Key, f.Value)
				}
			}

			fmt.Fprintf(b, "Filename: %s\nSize: %d\nMD5sum: %s\nSHA1: %s\nSHA256: %s\n\n", d.Paths(p)[0], p.Size, p.MD5, p.SHA1, p.SHA256)
		}
	}

	sums := make([]debFile, 0, 2*len(d.o.Archs))

	for _, a := range d.o.Archs {
		dir := path.Join(d.o.Component, "binary-"+a)
		plain := byArch[a].Bytes()

		for _, f := range []debFile{{path.Join(dir, "Packages"), plain}, {path.Join(dir, "Packages.gz"), gz(plain)}} {
			sums = append(sums, f)
			out[path.Join(dist, f.name)] = f.data
			h := sha256.Sum256(f.data)
			out[path.Join(dist, dir, "by-hash", "SHA256", hex.EncodeToString(h[:]))] = f.data
			m := md5.Sum(f.data) //nolint:gosec // apt lists MD5Sum too
			out[path.Join(dist, dir, "by-hash", "MD5Sum", hex.EncodeToString(m[:]))] = f.data
		}
	}

	return sums
}

// release is the suite's Release file over its index files.
func (d *deb) release(sums []debFile, at time.Time) []byte {
	var rel bytes.Buffer

	fmt.Fprintf(&rel, "Origin: %s\nLabel: %s\nSuite: %s\nCodename: %s\nDate: %s\n", d.o.Origin, d.o.Origin, d.o.Suite, d.o.Suite, at.UTC().Format(time.RFC1123))
	fmt.Fprintf(&rel, "Architectures: %s\nComponents: %s\nAcquire-By-Hash: yes\n", strings.Join(d.o.Archs, " "), d.o.Component)

	rel.WriteString("MD5Sum:\n")

	for _, s := range sums {
		m := md5.Sum(s.data) //nolint:gosec // apt lists MD5Sum too
		fmt.Fprintf(&rel, " %s %d %s\n", hex.EncodeToString(m[:]), len(s.data), s.name)
	}

	rel.WriteString("SHA256:\n")

	for _, s := range sums {
		h := sha256.Sum256(s.data)
		fmt.Fprintf(&rel, " %s %d %s\n", hex.EncodeToString(h[:]), len(s.data), s.name)
	}

	return rel.Bytes()
}
