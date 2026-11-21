package format

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
)

// arch is a pacman repository: a database per architecture.
type arch struct {
	o   Options
	pgp *sign.OpenPGP
}

func newArch(o Options, pgp *sign.OpenPGP) *arch {
	if o.Repo == "" {
		o.Repo = "piperun"
	}

	o.Archs = archsOr(o.Archs, []string{"x86_64", "aarch64"})

	return &arch{o: o, pgp: pgp}
}

func (a *arch) Name() string { return "arch" }

var archSuffixes = []string{".pkg.tar.zst", ".pkg.tar.xz", ".pkg.tar.gz"}

func (a *arch) Accept(file string) error {
	for _, s := range archSuffixes {
		if strings.HasSuffix(file, s) {
			return nil
		}
	}

	return fmt.Errorf("%w: %s: an Arch store takes %s files", ErrFormat, file, strings.Join(archSuffixes, ", "))
}

func (a *arch) Parse(file string, r io.ReaderAt, size int64) (*meta.Package, error) {
	z, err := decompress(file, io.NewSectionReader(r, 0, size))
	if err != nil {
		return nil, err
	}

	p := &meta.Package{File: file}
	found := false

	err = walkTar(z, func(h *tar.Header, body io.Reader) error {
		name := cleanPath(h.Name)
		if name == ".PKGINFO" {
			found = true

			b, err := io.ReadAll(io.LimitReader(body, 1<<20))
			if err != nil {
				return fmt.Errorf("%w: .PKGINFO: %w", ErrFormat, err)
			}

			p.Fields = parseKeyValues(string(b))

			return nil
		}

		if strings.HasPrefix(name, ".") {
			return nil
		}

		if f, ok := fileOf(h); ok {
			p.Files = append(p.Files, f)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("%w: %s: no .PKGINFO, not an Arch package", ErrFormat, file)
	}

	p.Name, p.Version, p.Arch = p.Get("pkgname"), p.Get("pkgver"), p.Get("arch")
	p.Description, p.URL = p.Get("pkgdesc"), p.Get("url")
	p.License = strings.Join(p.All("license"), " ")
	p.Depends = p.All("depend")

	if n, err := strconv.ParseInt(p.Get("size"), 10, 64); err == nil {
		p.Installed = n
	}

	if n, err := strconv.ParseInt(p.Get("builddate"), 10, 64); err == nil {
		p.Built = time.Unix(n, 0).UTC()
	}

	return p, p.Check()
}

// parseKeyValues reads "key = value" lines, as .PKGINFO holds them.
func parseKeyValues(s string) []meta.Field {
	var out []meta.Field

	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		out = append(out, meta.Field{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}

	return out
}

func (a *arch) Paths(p *meta.Package) []string {
	var out []string

	for _, ar := range archsOf(p.Arch, a.o.Archs, "any") {
		out = append(out, path.Join(ar, p.File))
	}

	return out
}

func (a *arch) Keys() (map[string][]byte, error) {
	k, err := a.pgp.PublicArmored()
	if err != nil {
		return nil, err
	}

	return map[string][]byte{"key.asc": k}, nil
}

func (a *arch) Immutable(string) bool { return false }

// desc is a package's desc file in a pacman database.
func desc(p *meta.Package) []byte {
	var b bytes.Buffer

	one := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%%%s%%\n%s\n\n", k, v)
		}
	}
	many := func(k string, vs []string) {
		if len(vs) > 0 {
			fmt.Fprintf(&b, "%%%s%%\n%s\n\n", k, strings.Join(vs, "\n"))
		}
	}

	base := p.Get("pkgbase")
	if base == "" {
		base = p.Name
	}

	one("FILENAME", p.File)
	one("NAME", p.Name)
	one("BASE", base)
	one("VERSION", p.Version)
	one("DESC", p.Description)
	many("GROUPS", p.All("group"))
	one("CSIZE", strconv.FormatInt(p.Size, 10))
	one("ISIZE", strconv.FormatInt(p.Installed, 10))
	one("MD5SUM", p.MD5)
	one("SHA256SUM", p.SHA256)
	one("URL", p.URL)
	many("LICENSE", p.All("license"))
	one("ARCH", p.Arch)
	one("BUILDDATE", p.Get("builddate"))
	one("PACKAGER", p.Get("packager"))
	many("REPLACES", p.All("replaces"))
	many("CONFLICTS", p.All("conflict"))
	many("PROVIDES", p.All("provides"))
	many("DEPENDS", p.All("depend"))
	many("OPTDEPENDS", p.All("optdepend"))
	many("MAKEDEPENDS", p.All("makedepend"))
	many("CHECKDEPENDS", p.All("checkdepend"))

	return b.Bytes()
}

// filesList is a package's files entry: paths without their leading
// slash, directories ending in one.
func filesList(p *meta.Package) []byte {
	var b bytes.Buffer

	b.WriteString("%FILES%\n")

	for _, f := range p.Files {
		name := strings.TrimPrefix(f.Path, "/")
		if f.Dir {
			name += "/"
		}

		b.WriteString(name + "\n")
	}

	b.WriteString("\n")

	return b.Bytes()
}

func (a *arch) Index(pkgs []*meta.Package, at time.Time) (map[string][]byte, error) {
	out := map[string][]byte{}

	for _, ar := range a.o.Archs {
		var db, files []tarEntry

		for _, p := range sortPackages(pkgs) {
			if p.Arch != ar && p.Arch != "any" {
				continue
			}

			dir := p.Name + "-" + p.Version + "/"
			d := desc(p)
			db = append(db, tarEntry{name: dir, dir: true}, tarEntry{name: dir + "desc", data: d})
			files = append(files, tarEntry{name: dir, dir: true}, tarEntry{name: dir + "desc", data: d}, tarEntry{name: dir + "files", data: filesList(p)})
		}

		for kind, entries := range map[string][]tarEntry{"db": db, "files": files} {
			data := gz(tarOf(entries, at, true))

			sig, err := a.pgp.Detach(data, false)
			if err != nil {
				return nil, err
			}

			for _, name := range []string{a.o.Repo + "." + kind, a.o.Repo + "." + kind + ".tar.gz"} {
				out[path.Join(ar, name)] = data
				out[path.Join(ar, name+".sig")] = sig
			}
		}
	}

	return out, nil
}
