package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
)

// rpm is a dnf repository: repodata signed in repomd.xml.asc.
type rpm struct {
	o   Options
	pgp *sign.OpenPGP
}

func newRPM(o Options, pgp *sign.OpenPGP) *rpm { return &rpm{o: o, pgp: pgp} }

func (r *rpm) Name() string { return "rpm" }

func (r *rpm) Accept(file string) error {
	if !strings.HasSuffix(file, ".rpm") || strings.HasSuffix(file, ".src.rpm") {
		return fmt.Errorf("%w: %s: an RPM store takes binary .rpm files", ErrFormat, file)
	}

	return nil
}

// The header tags piperepo reads.
const (
	tagName            = 1000
	tagVersion         = 1001
	tagRelease         = 1002
	tagEpoch           = 1003
	tagSummary         = 1004
	tagDescription     = 1005
	tagBuildTime       = 1006
	tagBuildHost       = 1007
	tagSize            = 1009
	tagVendor          = 1011
	tagLicense         = 1014
	tagPackager        = 1015
	tagGroup           = 1016
	tagURL             = 1020
	tagArch            = 1022
	tagFileSizes       = 1028
	tagFileModes       = 1030
	tagSourceRPM       = 1044
	tagProvideName     = 1047
	tagRequireFlags    = 1048
	tagRequireName     = 1049
	tagRequireVersion  = 1050
	tagConflictFlags   = 1053
	tagConflictName    = 1054
	tagConflictVersion = 1055
	tagObsoleteName    = 1090
	tagProvideFlags    = 1112
	tagProvideVersion  = 1113
	tagObsoleteFlags   = 1114
	tagObsoleteVersion = 1115
	tagDirIndexes      = 1116
	tagBaseNames       = 1117
	tagDirNames        = 1118
	tagLongFileSizes   = 5008
	tagLongSize        = 5009
	sigPayloadSize     = 1007
	sigLongArchiveSize = 271
)

// rpmHeader is one header's entries, by tag.
type rpmHeader map[int32]any

var headerMagic = []byte{0x8e, 0xad, 0xe8, 0x01}

// readHeader reads the header at off: its entries and its length.
func readHeader(r io.ReaderAt, off, size int64) (rpmHeader, int64, error) {
	pre := make([]byte, 16)
	if _, err := r.ReadAt(pre, off); err != nil || !bytes.Equal(pre[:4], headerMagic) {
		return nil, 0, fmt.Errorf("%w: no rpm header at %d", ErrFormat, off)
	}

	n := int64(binary.BigEndian.Uint32(pre[8:12]))
	hsize := int64(binary.BigEndian.Uint32(pre[12:16]))

	length := 16 + n*16 + hsize
	if n > 1<<16 || hsize > 256<<20 || off+length > size {
		return nil, 0, fmt.Errorf("%w: an rpm header's size", ErrFormat)
	}

	buf := make([]byte, length-16)
	if _, err := r.ReadAt(buf, off+16); err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrFormat, err)
	}

	store := buf[n*16:]
	h := rpmHeader{}

	for i := range n {
		e := buf[i*16 : i*16+16]
		tag := int32(binary.BigEndian.Uint32(e[0:4])) //nolint:gosec // tags fit
		typ := binary.BigEndian.Uint32(e[4:8])
		at := int64(binary.BigEndian.Uint32(e[8:12]))
		count := int64(binary.BigEndian.Uint32(e[12:16]))

		if at > int64(len(store)) {
			continue
		}

		if v := entryValue(store[at:], typ, count); v != nil {
			h[tag] = v
		}
	}

	return h, length, nil
}

// entryValue reads an entry's value: strings, or numbers as int64s.
func entryValue(b []byte, typ uint32, count int64) any {
	ints := func(width int64) []int64 {
		if count*width > int64(len(b)) {
			return nil
		}

		out := make([]int64, count)

		for i := range count {
			v := b[i*width : i*width+width]

			switch width {
			case 1:
				out[i] = int64(v[0])
			case 2:
				out[i] = int64(binary.BigEndian.Uint16(v))
			case 4:
				out[i] = int64(binary.BigEndian.Uint32(v))
			default:
				out[i] = int64(binary.BigEndian.Uint64(v)) //nolint:gosec // sizes fit
			}
		}

		return out
	}

	switch typ {
	case 2:
		return ints(1)
	case 3:
		return ints(2)
	case 4:
		return ints(4)
	case 5:
		return ints(8)
	case 6, 8, 9:
		var out []string

		for range count {
			end := bytes.IndexByte(b, 0)
			if end < 0 {
				break
			}

			out = append(out, string(b[:end]))
			b = b[end+1:]
		}

		return out
	default:
		return nil
	}
}

func (h rpmHeader) str(tag int32) string {
	if s, ok := h[tag].([]string); ok && len(s) > 0 {
		return s[0]
	}

	return ""
}

func (h rpmHeader) strs(tag int32) []string {
	s, _ := h[tag].([]string) //nolint:errcheck // absent is none

	return s
}

func (h rpmHeader) ints(tag int32) []int64 {
	n, _ := h[tag].([]int64) //nolint:errcheck // absent is none

	return n
}

func (h rpmHeader) num(tag int32) (int64, bool) {
	if n := h.ints(tag); len(n) > 0 {
		return n[0], true
	}

	return 0, false
}

func (r *rpm) Parse(file string, ra io.ReaderAt, size int64) (*meta.Package, error) {
	lead := make([]byte, 96)
	if _, err := ra.ReadAt(lead, 0); err != nil || !bytes.Equal(lead[:4], []byte{0xed, 0xab, 0xee, 0xdb}) {
		return nil, fmt.Errorf("%w: %s: not an rpm package", ErrFormat, file)
	}

	sig, sigLen, err := readHeader(ra, 96, size)
	if err != nil {
		return nil, err
	}

	start := 96 + sigLen
	start += (8 - start%8) % 8

	h, hLen, err := readHeader(ra, start, size)
	if err != nil {
		return nil, err
	}

	info := &meta.RPM{
		Ver: h.str(tagVersion), Rel: h.str(tagRelease), Epoch: "0", Summary: h.str(tagSummary),
		Packager: h.str(tagPackager), Vendor: h.str(tagVendor), Group: h.str(tagGroup), BuildHost: h.str(tagBuildHost),
		SourceRPM: h.str(tagSourceRPM), HeaderStart: start, HeaderEnd: start + hLen,
		Provides:  deps(h, tagProvideName, tagProvideFlags, tagProvideVersion),
		Requires:  deps(h, tagRequireName, tagRequireFlags, tagRequireVersion),
		Conflicts: deps(h, tagConflictName, tagConflictFlags, tagConflictVersion),
		Obsoletes: deps(h, tagObsoleteName, tagObsoleteFlags, tagObsoleteVersion),
	}

	if e, ok := h.num(tagEpoch); ok {
		info.Epoch = strconv.FormatInt(e, 10)
	}

	if n, ok := sig.num(sigLongArchiveSize); ok {
		info.Archive = n
	} else if n, ok := sig.num(sigPayloadSize); ok {
		info.Archive = n
	}

	p := &meta.Package{
		File: file, Name: h.str(tagName), Arch: h.str(tagArch), Description: info.Summary,
		License: h.str(tagLicense), URL: h.str(tagURL), RPM: info, Files: rpmFiles(h),
		Fields: []meta.Field{{Key: "description", Value: h.str(tagDescription)}},
	}

	p.Version = info.Ver + "-" + info.Rel
	if info.Epoch != "0" {
		p.Version = info.Epoch + ":" + p.Version
	}

	for _, d := range info.Requires {
		if !strings.HasPrefix(d.Name, "rpmlib(") {
			p.Depends = append(p.Depends, d.Name)
		}
	}

	if n, ok := h.num(tagLongSize); ok {
		p.Installed = n
	} else if n, ok := h.num(tagSize); ok {
		p.Installed = n
	}

	if n, ok := h.num(tagBuildTime); ok {
		p.Built = time.Unix(n, 0).UTC()
	}

	return p, p.Check()
}

// rpmlibFlag marks a requirement on rpm itself, which repodata leaves out.
const rpmlibFlag = 1 << 24

// deps are a header's relations of one kind.
func deps(h rpmHeader, nameTag, flagTag, verTag int32) []meta.Dep {
	names, flags, vers := h.strs(nameTag), h.ints(flagTag), h.strs(verTag)

	var out []meta.Dep

	for i, n := range names {
		d := meta.Dep{Name: n}

		var f int64
		if i < len(flags) {
			f = flags[i]
		}

		if f&rpmlibFlag != 0 {
			continue
		}

		d.Pre = f&(1<<9|1<<6) != 0

		switch f & 0xe {
		case 2:
			d.Flags = "LT"
		case 4:
			d.Flags = "GT"
		case 8:
			d.Flags = "EQ"
		case 10:
			d.Flags = "LE"
		case 12:
			d.Flags = "GE"
		}

		if i < len(vers) && vers[i] != "" && d.Flags != "" {
			d.Epoch, d.Ver, d.Rel = evr(vers[i])
		}

		out = append(out, d)
	}

	return out
}

// evr splits [epoch:]version[-release].
func evr(s string) (string, string, string) {
	epoch := ""
	if e, rest, ok := strings.Cut(s, ":"); ok {
		epoch, s = e, rest
	}

	ver, rel := s, ""
	if i := strings.LastIndex(s, "-"); i >= 0 {
		ver, rel = s[:i], s[i+1:]
	}

	if epoch == "" {
		epoch = "0"
	}

	return epoch, ver, rel
}

// rpmFiles are a header's files, directories marked.
func rpmFiles(h rpmHeader) []meta.File {
	base, dirs, idx := h.strs(tagBaseNames), h.strs(tagDirNames), h.ints(tagDirIndexes)
	sizes, modes := h.ints(tagLongFileSizes), h.ints(tagFileModes)

	if len(sizes) == 0 {
		sizes = h.ints(tagFileSizes)
	}

	var out []meta.File

	for i, b := range base {
		if i >= len(idx) || idx[i] < 0 || idx[i] >= int64(len(dirs)) {
			continue
		}

		f := meta.File{Path: dirs[idx[i]] + b}
		if i < len(sizes) {
			f.Size = sizes[i]
		}

		if i < len(modes) && modes[i]&0o170000 == 0o040000 {
			f.Dir, f.Size = true, 0
		}

		out = append(out, f)
	}

	return out
}

func (r *rpm) Paths(p *meta.Package) []string { return []string{path.Join("packages", p.File)} }

func (r *rpm) Keys() (map[string][]byte, error) {
	k, err := r.pgp.PublicArmored()
	if err != nil {
		return nil, err
	}

	return map[string][]byte{"key.asc": k, "repodata/repomd.xml.key": k}, nil
}

func (r *rpm) Immutable(p string) bool {
	return strings.HasPrefix(p, "repodata/") && !strings.HasPrefix(p, "repodata/repomd.xml")
}

// primaryFile is a file primary.xml lists: those dependencies name.
var primaryFile = regexp.MustCompile(`^(/etc/|.*bin/|/usr/lib/sendmail$)`)

func esc(s string) string {
	var b strings.Builder

	_ = xml.EscapeText(&b, []byte(s)) //nolint:errcheck // a builder

	return b.String()
}

func versionAttrs(info *meta.RPM) string {
	return fmt.Sprintf(`epoch="%s" ver="%s" rel="%s"`, esc(info.Epoch), esc(info.Ver), esc(info.Rel))
}

func writeDeps(b *bytes.Buffer, kind string, ds []meta.Dep) {
	if len(ds) == 0 {
		return
	}

	fmt.Fprintf(b, "    <rpm:%s>\n", kind)

	for _, d := range ds {
		fmt.Fprintf(b, `      <rpm:entry name="%s"`, esc(d.Name))

		if d.Flags != "" {
			fmt.Fprintf(b, ` flags="%s" epoch="%s" ver="%s"`, d.Flags, esc(d.Epoch), esc(d.Ver))

			if d.Rel != "" {
				fmt.Fprintf(b, ` rel="%s"`, esc(d.Rel))
			}
		}

		if d.Pre {
			b.WriteString(` pre="1"`)
		}

		b.WriteString("/>\n")
	}

	fmt.Fprintf(b, "    </rpm:%s>\n", kind)
}

func (r *rpm) primary(pkgs []*meta.Package) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata xmlns=\"http://linux.duke.edu/metadata/common\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\" packages=\"%d\">\n", len(pkgs))

	for _, p := range pkgs {
		i := p.RPM
		fmt.Fprintf(&b, "<package type=\"rpm\">\n  <name>%s</name>\n  <arch>%s</arch>\n  <version %s/>\n", esc(p.Name), esc(p.Arch), versionAttrs(i))
		fmt.Fprintf(&b, "  <checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>\n  <summary>%s</summary>\n  <description>%s</description>\n", p.SHA256, esc(i.Summary), esc(p.Get("description")))
		fmt.Fprintf(&b, "  <packager>%s</packager>\n  <url>%s</url>\n", esc(i.Packager), esc(p.URL))
		fmt.Fprintf(&b, "  <time file=\"%d\" build=\"%d\"/>\n", p.Uploaded.Unix(), p.Built.Unix())
		fmt.Fprintf(&b, "  <size package=\"%d\" installed=\"%d\" archive=\"%d\"/>\n", p.Size, p.Installed, i.Archive)
		fmt.Fprintf(&b, "  <location href=\"%s\"/>\n  <format>\n", esc(r.Paths(p)[0]))
		fmt.Fprintf(&b, "    <rpm:license>%s</rpm:license>\n    <rpm:vendor>%s</rpm:vendor>\n    <rpm:group>%s</rpm:group>\n", esc(p.License), esc(i.Vendor), esc(i.Group))
		fmt.Fprintf(&b, "    <rpm:buildhost>%s</rpm:buildhost>\n    <rpm:sourcerpm>%s</rpm:sourcerpm>\n", esc(i.BuildHost), esc(i.SourceRPM))
		fmt.Fprintf(&b, "    <rpm:header-range start=\"%d\" end=\"%d\"/>\n", i.HeaderStart, i.HeaderEnd)
		writeDeps(&b, "provides", i.Provides)
		writeDeps(&b, "requires", i.Requires)
		writeDeps(&b, "conflicts", i.Conflicts)
		writeDeps(&b, "obsoletes", i.Obsoletes)

		for _, f := range p.Files {
			if !f.Dir && primaryFile.MatchString(f.Path) {
				fmt.Fprintf(&b, "    <file>%s</file>\n", esc(f.Path))
			}
		}

		b.WriteString("  </format>\n</package>\n")
	}

	b.WriteString("</metadata>\n")

	return b.Bytes()
}

func filelists(pkgs []*meta.Package) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<filelists xmlns=\"http://linux.duke.edu/metadata/filelists\" packages=\"%d\">\n", len(pkgs))

	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n  <version %s/>\n", p.SHA256, esc(p.Name), esc(p.Arch), versionAttrs(p.RPM))

		for _, f := range p.Files {
			if f.Dir {
				fmt.Fprintf(&b, "  <file type=\"dir\">%s</file>\n", esc(f.Path))
			} else {
				fmt.Fprintf(&b, "  <file>%s</file>\n", esc(f.Path))
			}
		}

		b.WriteString("</package>\n")
	}

	b.WriteString("</filelists>\n")

	return b.Bytes()
}

func other(pkgs []*meta.Package) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<otherdata xmlns=\"http://linux.duke.edu/metadata/other\" packages=\"%d\">\n", len(pkgs))

	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n  <version %s/>\n</package>\n", p.SHA256, esc(p.Name), esc(p.Arch), versionAttrs(p.RPM))
	}

	b.WriteString("</otherdata>\n")

	return b.Bytes()
}

func (r *rpm) Index(pkgs []*meta.Package, at time.Time) (map[string][]byte, error) {
	var with []*meta.Package

	for _, p := range sortPackages(pkgs) {
		if p.RPM != nil {
			with = append(with, p)
		}
	}

	out := map[string][]byte{}

	var md bytes.Buffer

	fmt.Fprintf(&md, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<repomd xmlns=\"http://linux.duke.edu/metadata/repo\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\">\n  <revision>%d</revision>\n", at.Unix())

	for _, d := range []struct {
		kind string
		data []byte
	}{{"primary", r.primary(with)}, {"filelists", filelists(with)}, {"other", other(with)}} {
		packed := gz(d.data)
		sum, open := sha256.Sum256(packed), sha256.Sum256(d.data)
		name := path.Join("repodata", hex.EncodeToString(sum[:])+"-"+d.kind+".xml.gz")
		out[name] = packed

		fmt.Fprintf(&md, "  <data type=\"%s\">\n    <checksum type=\"sha256\">%s</checksum>\n    <open-checksum type=\"sha256\">%s</open-checksum>\n", d.kind, hex.EncodeToString(sum[:]), hex.EncodeToString(open[:]))
		fmt.Fprintf(&md, "    <location href=\"%s\"/>\n    <timestamp>%d</timestamp>\n    <size>%d</size>\n    <open-size>%d</open-size>\n  </data>\n", name, at.Unix(), len(packed), len(d.data))
	}

	md.WriteString("</repomd>\n")

	sig, err := r.pgp.Detach(md.Bytes(), true)
	if err != nil {
		return nil, err
	}

	out["repodata/repomd.xml"] = md.Bytes()
	out["repodata/repomd.xml.asc"] = sig

	return out, nil
}
