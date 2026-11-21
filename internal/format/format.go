// Package format reads each package format's files and writes its index:
// apt's, pacman's, apk's and rpm's, signed, in Go alone.
package format

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
)

// Format is one package format: how its files are read, where they are
// served, and the index that lists them.
type Format interface {
	Name() string
	// Accept refuses a file name the format doesn't take.
	Accept(file string) error
	// Parse reads a package's own metadata; the hashes are the caller's.
	Parse(file string, r io.ReaderAt, size int64) (*meta.Package, error)
	// Paths are where a package is served; one in several architectures
	// is served in each.
	Paths(p *meta.Package) []string
	// Index is every index file by its served path, signed.
	Index(pkgs []*meta.Package, at time.Time) (map[string][]byte, error)
	// Keys are the public key files served, by path.
	Keys() (map[string][]byte, error)
	// Immutable is whether an index path's content never changes.
	Immutable(path string) bool
}

// Options are a store's own words for its format.
type Options struct {
	// Suite and Component are apt's; Archs every format's architectures,
	// those a package of any architecture is listed under.
	Suite     string
	Component string
	Archs     []string
	// Repo is pacman's database name, KeyName apk's key file name.
	Repo    string
	KeyName string
	Origin  string
}

// Signer holds the key a format signs with.
type Signer struct {
	PGP *sign.OpenPGP
	RSA *sign.RSA
}

// Names are the formats there are.
var Names = []string{"apt", "arch", "apk", "rpm"}

// ErrFormat is a package or a name a format refuses.
var ErrFormat = errors.New("format")

// New is the format named, with its defaults filled in.
func New(name string, o Options, s Signer) (Format, error) {
	if o.Origin == "" {
		o.Origin = "piperepo"
	}

	switch name {
	case "apt":
		if s.PGP == nil {
			return nil, fmt.Errorf("%w: apt signs with an OpenPGP key", ErrFormat)
		}

		return newDeb(o, s.PGP), nil
	case "arch":
		if s.PGP == nil {
			return nil, fmt.Errorf("%w: arch signs with an OpenPGP key", ErrFormat)
		}

		return newArch(o, s.PGP), nil
	case "apk":
		if s.RSA == nil {
			return nil, fmt.Errorf("%w: apk signs with an RSA key", ErrFormat)
		}

		return newAPK(o, s.RSA), nil
	case "rpm":
		if s.PGP == nil {
			return nil, fmt.Errorf("%w: rpm signs with an OpenPGP key", ErrFormat)
		}

		return newRPM(o, s.PGP), nil
	default:
		return nil, fmt.Errorf("%w: %q: one of %s", ErrFormat, name, strings.Join(Names, ", "))
	}
}

// OpenPGP is whether a format signs with an OpenPGP key, else an RSA one.
func OpenPGP(name string) bool { return name != "apk" }

func archsOr(given, def []string) []string {
	if len(given) > 0 {
		return given
	}

	return def
}

// archsOf are the architectures a package is listed under: its own, or
// every one for a package of any.
func archsOf(arch string, every []string, all string) []string {
	if arch == all {
		return every
	}

	return []string{arch}
}

// decompress reads a stream compressed as its name or first bytes say.
func decompress(name string, r io.Reader) (io.Reader, error) {
	var head [6]byte

	br := &peekReader{r: r}

	n, _ := io.ReadFull(br, head[:]) //nolint:errcheck // a short stream is plain
	br.buf = append([]byte(nil), head[:n]...)

	switch {
	case bytes.HasPrefix(head[:n], []byte{0x1f, 0x8b}):
		z, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrFormat, name, err)
		}

		return z, nil
	case bytes.HasPrefix(head[:n], []byte{0x28, 0xb5, 0x2f, 0xfd}):
		z, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrFormat, name, err)
		}

		return z.IOReadCloser(), nil
	case bytes.HasPrefix(head[:n], []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		z, err := xz.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrFormat, name, err)
		}

		return z, nil
	default:
		return br, nil
	}
}

type peekReader struct {
	r   io.Reader
	buf []byte
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]

		return n, nil
	}

	return p.r.Read(b) //nolint:wrapcheck // the stream's own
}

// walkTar calls fn with each entry of a tar stream.
func walkTar(r io.Reader, fn func(h *tar.Header, body io.Reader) error) error {
	tr := tar.NewReader(r)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("%w: tar: %w", ErrFormat, err)
		}

		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// cleanPath is a tar entry's path without its leading ./ or /.
func cleanPath(name string) string {
	name = strings.TrimPrefix(name, "./")

	return strings.TrimPrefix(name, "/")
}

// fileOf is a tar entry as a package's file, or false for what isn't one.
func fileOf(h *tar.Header) (meta.File, bool) {
	p := strings.TrimSuffix(cleanPath(h.Name), "/")
	if p == "" || p == "." {
		return meta.File{}, false
	}

	switch h.Typeflag {
	case tar.TypeDir:
		return meta.File{Path: "/" + p, Dir: true}, true
	case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
		return meta.File{Path: "/" + p, Size: h.Size}, true
	default:
		return meta.File{}, false
	}
}

// gz is data gzipped, the same bytes every time.
func gz(data []byte) []byte {
	var b bytes.Buffer

	w, _ := gzip.NewWriterLevel(&b, gzip.BestCompression) //nolint:errcheck // a valid level
	_, _ = w.Write(data)                                  //nolint:errcheck // a buffer
	_ = w.Close()                                         //nolint:errcheck // a buffer

	return b.Bytes()
}

// tarEntry is one file of a tar written here.
type tarEntry struct {
	name string
	dir  bool
	data []byte
}

// tarOf is a tar of entries, each owned by root and dated at; end writes
// the archive's end, which a signature segment leaves out.
func tarOf(entries []tarEntry, at time.Time, end bool) []byte {
	var b bytes.Buffer

	tw := tar.NewWriter(&b)

	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), ModTime: at, Typeflag: tar.TypeReg, Uname: "root", Gname: "root", Format: tar.FormatUSTAR}
		if e.dir {
			h.Mode, h.Size, h.Typeflag = 0o755, 0, tar.TypeDir
		}

		_ = tw.WriteHeader(h)   //nolint:errcheck // a buffer
		_, _ = tw.Write(e.data) //nolint:errcheck // a buffer
	}

	if end {
		_ = tw.Close() //nolint:errcheck // a buffer
	} else {
		_ = tw.Flush() //nolint:errcheck // a buffer
	}

	return b.Bytes()
}

// sortPackages orders packages by name, then version, then file.
func sortPackages(pkgs []*meta.Package) []*meta.Package {
	out := slices.Clone(pkgs)
	slices.SortFunc(out, func(a, b *meta.Package) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		if c := strings.Compare(a.Version, b.Version); c != 0 {
			return c
		}

		return strings.Compare(a.File, b.File)
	})

	return out
}

// oneLine is the first line of a text.
func oneLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(line)
}
