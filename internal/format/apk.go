package format

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/sha1" //nolint:gosec // apk names a package by its control segment's SHA-1
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
)

// apk is an Alpine repository: an APKINDEX per architecture.
type apk struct {
	o   Options
	rsa *sign.RSA
}

func newAPK(o Options, r *sign.RSA) *apk {
	if o.KeyName == "" {
		o.KeyName = "piperun.rsa.pub"
	}

	o.Archs = archsOr(o.Archs, []string{"x86_64", "aarch64"})

	return &apk{o: o, rsa: r}
}

func (a *apk) Name() string { return "apk" }

func (a *apk) Accept(file string) error {
	if !strings.HasSuffix(file, ".apk") {
		return fmt.Errorf("%w: %s: an Alpine store takes .apk files", ErrFormat, file)
	}

	return nil
}

// counted counts what a reader read.
type counted struct {
	r io.Reader
	n int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err //nolint:wrapcheck // the reader's own
}

// segment is one gzip member of an .apk: where it starts and ends.
type segment struct{ start, end int64 }

// apkSegments reads each gzip member of an .apk in turn, calling fn with
// its tar, and answers where each lies.
func apkSegments(r io.ReaderAt, size int64, fn func(i int, body io.Reader) error) ([]segment, error) {
	c := &counted{r: io.NewSectionReader(r, 0, size)}
	br := bufio.NewReader(c)

	var segs []segment

	for i := 0; ; i++ {
		start := c.n - int64(br.Buffered())
		if start >= size {
			return segs, nil
		}

		z, err := gzip.NewReader(br)
		if err != nil {
			if i > 0 && errors.Is(err, io.EOF) {
				return segs, nil
			}

			return nil, fmt.Errorf("%w: not an apk v2 package (gzip members): %w", ErrFormat, err)
		}

		z.Multistream(false)

		if err := fn(i, z); err != nil {
			return nil, err
		}

		// What the tar left unread belongs to the member still.
		if _, err := io.Copy(io.Discard, z); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFormat, err)
		}

		segs = append(segs, segment{start: start, end: c.n - int64(br.Buffered())})
	}
}

func (a *apk) Parse(file string, r io.ReaderAt, size int64) (*meta.Package, error) {
	head := make([]byte, 4)
	if _, err := r.ReadAt(head, 0); err == nil && string(head[:3]) == "ADB" {
		return nil, fmt.Errorf("%w: %s: an apk v3 package; build it with abuild, which makes v2 ones", ErrFormat, file)
	}

	p := &meta.Package{File: file}
	control := -1

	segs, err := apkSegments(r, size, func(i int, body io.Reader) error {
		return walkTar(body, func(h *tar.Header, b io.Reader) error {
			name := cleanPath(h.Name)

			switch {
			case strings.HasPrefix(name, ".SIGN."):
			case name == ".PKGINFO":
				control = i

				data, err := io.ReadAll(io.LimitReader(b, 1<<20))
				if err != nil {
					return fmt.Errorf("%w: .PKGINFO: %w", ErrFormat, err)
				}

				p.Fields = parseKeyValues(string(data))
			case strings.HasPrefix(name, "."):
			case control >= 0 && i > control:
				if f, ok := fileOf(h); ok {
					p.Files = append(p.Files, f)
				}
			}

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	if control < 0 || control >= len(segs) {
		return nil, fmt.Errorf("%w: %s: no .PKGINFO, not an Alpine package", ErrFormat, file)
	}

	ctl := make([]byte, segs[control].end-segs[control].start)
	if _, err := r.ReadAt(ctl, segs[control].start); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFormat, err)
	}

	sum := sha1.Sum(ctl) //nolint:gosec // apk's own identity of a package
	p.APKHash = "Q1" + base64.StdEncoding.EncodeToString(sum[:])

	p.Name, p.Version, p.Arch = p.Get("pkgname"), p.Get("pkgver"), p.Get("arch")
	p.Description, p.URL, p.License = p.Get("pkgdesc"), p.Get("url"), p.Get("license")
	p.Depends = p.All("depend")

	if n, err := strconv.ParseInt(p.Get("size"), 10, 64); err == nil {
		p.Installed = n
	}

	if n, err := strconv.ParseInt(p.Get("builddate"), 10, 64); err == nil {
		p.Built = time.Unix(n, 0).UTC()
	}

	return p, p.Check()
}

// Paths are where apk fetches a package: its name and version, whatever
// it was uploaded as.
func (a *apk) Paths(p *meta.Package) []string {
	var out []string

	for _, ar := range archsOf(p.Arch, a.o.Archs, "noarch") {
		out = append(out, path.Join(ar, p.Name+"-"+p.Version+".apk"))
	}

	return out
}

func (a *apk) Keys() (map[string][]byte, error) {
	k, err := a.rsa.PublicPEM()
	if err != nil {
		return nil, err
	}

	return map[string][]byte{"keys/" + a.o.KeyName: k, "key.rsa.pub": k}, nil
}

func (a *apk) Immutable(string) bool { return false }

// apkEntry is a package's entry in an APKINDEX.
func apkEntry(p *meta.Package) []byte {
	var b bytes.Buffer

	one := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s:%s\n", k, strings.ReplaceAll(v, "\n", " "))
		}
	}

	one("C", p.APKHash)
	one("P", p.Name)
	one("V", p.Version)
	one("A", p.Arch)
	one("S", strconv.FormatInt(p.Size, 10))
	one("I", strconv.FormatInt(p.Installed, 10))
	one("T", p.Description)
	one("U", p.URL)
	one("L", p.License)
	one("o", p.Get("origin"))
	one("m", p.Get("maintainer"))
	one("t", p.Get("builddate"))
	one("c", p.Get("commit"))
	one("D", strings.Join(p.All("depend"), " "))
	one("p", strings.Join(p.All("provides"), " "))
	one("i", strings.Join(p.All("install_if"), " "))
	one("r", strings.Join(p.All("replaces"), " "))
	b.WriteString("\n")

	return b.Bytes()
}

func (a *apk) Index(pkgs []*meta.Package, at time.Time) (map[string][]byte, error) {
	out := map[string][]byte{}

	for _, ar := range a.o.Archs {
		var idx bytes.Buffer

		for _, p := range sortPackages(pkgs) {
			if p.Arch == ar || p.Arch == "noarch" {
				idx.Write(apkEntry(p))
			}
		}

		body := gz(tarOf([]tarEntry{{name: "DESCRIPTION", data: []byte(a.o.Origin)}, {name: "APKINDEX", data: idx.Bytes()}}, at, true))

		sig, err := a.rsa.Sign(body, crypto.SHA256)
		if err != nil {
			return nil, err
		}

		head := gz(tarOf([]tarEntry{{name: ".SIGN.RSA256." + a.o.KeyName, data: sig}}, at, false))
		out[path.Join(ar, "APKINDEX.tar.gz")] = append(head, body...)
	}

	return out, nil
}
