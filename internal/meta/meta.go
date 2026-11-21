// Package meta is what piperepo knows of one package: read from the
// package once at upload and kept as a small object beside it, so an index
// is rebuilt from these alone.
package meta

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Package is one package file and what its format says of it.
type Package struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Arch        string    `json:"arch"`
	File        string    `json:"file"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	SHA1        string    `json:"sha1"`
	MD5         string    `json:"md5"`
	Description string    `json:"description,omitempty"`
	License     string    `json:"license,omitempty"`
	URL         string    `json:"url,omitempty"`
	Depends     []string  `json:"depends,omitempty"`
	Installed   int64     `json:"installed,omitempty"`
	Built       time.Time `json:"built,omitzero"`
	Uploaded    time.Time `json:"uploaded"`
	// ETag is the package object's, so a meta object of other bytes is known stale.
	ETag string `json:"etag"`
	// Fields are the format's own fields in their order: a .deb's control
	// paragraph, a .PKGINFO's lines.
	Fields []Field `json:"fields,omitempty"`
	// APKHash is an .apk's control segment's SHA-1, as APKINDEX names it.
	APKHash string `json:"apk_hash,omitempty"`
	RPM     *RPM   `json:"rpm,omitempty"`
	Files   []File `json:"files,omitempty"`
}

// Field is one key and value of a package's own metadata.
type Field struct {
	Key   string `json:"k"`
	Value string `json:"v"`
}

// File is one file a package installs.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir,omitempty"`
}

// RPM is what repodata needs of an .rpm beyond the common fields.
type RPM struct {
	Epoch       string `json:"epoch"`
	Ver         string `json:"ver"`
	Rel         string `json:"rel"`
	Summary     string `json:"summary"`
	Packager    string `json:"packager,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
	Group       string `json:"group,omitempty"`
	BuildHost   string `json:"build_host,omitempty"`
	SourceRPM   string `json:"source_rpm,omitempty"`
	HeaderStart int64  `json:"header_start"`
	HeaderEnd   int64  `json:"header_end"`
	Archive     int64  `json:"archive,omitempty"`
	Provides    []Dep  `json:"provides,omitempty"`
	Requires    []Dep  `json:"requires,omitempty"`
	Conflicts   []Dep  `json:"conflicts,omitempty"`
	Obsoletes   []Dep  `json:"obsoletes,omitempty"`
}

// Dep is an rpm relation: a name, and a version it compares to.
type Dep struct {
	Name  string `json:"name"`
	Flags string `json:"flags,omitempty"`
	Epoch string `json:"epoch,omitempty"`
	Ver   string `json:"ver,omitempty"`
	Rel   string `json:"rel,omitempty"`
	Pre   bool   `json:"pre,omitempty"`
}

// Get is a field's first value, or "".
func (p *Package) Get(key string) string {
	for _, f := range p.Fields {
		if f.Key == key {
			return f.Value
		}
	}

	return ""
}

// All are a field's values in order.
func (p *Package) All(key string) []string {
	var out []string

	for _, f := range p.Fields {
		if f.Key == key {
			out = append(out, f.Value)
		}
	}

	return out
}

var (
	fileRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,254}$`)
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+@-]{0,254}$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~:-]{0,254}$`)
)

// ErrInvalid is a package or a name piperepo refuses.
var ErrInvalid = errors.New("invalid")

// FileName says whether a file name may be stored and served.
func FileName(name string) error {
	if !fileRE.MatchString(name) {
		return fmt.Errorf("%w: file name %q: letters, digits and ._+~- only", ErrInvalid, name)
	}

	return nil
}

// Check refuses a package whose name, version or arch would be unsafe in a
// path or an index.
func (p *Package) Check() error {
	switch {
	case !nameRE.MatchString(p.Name):
		return fmt.Errorf("%w: package name %q", ErrInvalid, p.Name)
	case !versionRE.MatchString(p.Version):
		return fmt.Errorf("%w: version %q", ErrInvalid, p.Version)
	case !nameRE.MatchString(p.Arch):
		return fmt.Errorf("%w: architecture %q", ErrInvalid, p.Arch)
	}

	return nil
}

// Entry is a package as an index generation lists it.
type Entry struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Arch        string    `json:"arch"`
	File        string    `json:"file"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Description string    `json:"description"`
	License     string    `json:"license"`
	Uploaded    time.Time `json:"uploaded"`
}

// EntryOf is p as a generation lists it, served at path.
func EntryOf(p *Package, path string) Entry {
	return Entry{
		Name: p.Name, Version: p.Version, Arch: p.Arch, File: p.File, Path: path, Size: p.Size, SHA256: p.SHA256,
		Description: p.Description, License: p.License, Uploaded: p.Uploaded,
	}
}

// Listed is a package as GET /api/packages lists it.
type Listed struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Arch        string    `json:"arch"`
	File        string    `json:"file"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Description string    `json:"description"`
	License     string    `json:"license"`
	Uploaded    time.Time `json:"uploaded"`
}

// ListedOf is an entry as the API lists it.
func ListedOf(e Entry) Listed {
	return Listed{
		Name: e.Name, Version: e.Version, Arch: e.Arch, File: e.File, Size: e.Size, SHA256: e.SHA256,
		Description: e.Description, License: e.License, Uploaded: e.Uploaded,
	}
}

// Version is one version of a package, as GET /api/packages/<name>/<version>
// answers: every architecture it is in, the first one's files.
type Version struct {
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Archs       []string   `json:"archs"`
	Description string     `json:"description"`
	License     string     `json:"license"`
	Size        int64      `json:"size"`
	Depends     []Relation `json:"depends"`
	Files       []File     `json:"files"`
}

// Relation is a dependency: a name, and the versions it takes as written.
type Relation struct {
	Name  string `json:"name"`
	Range string `json:"range"`
}

// rpmOps are rpm's comparison flags as written.
var rpmOps = map[string]string{"LT": "<", "GT": ">", "EQ": "=", "LE": "<=", "GE": ">="}

// Relations are p's dependencies, each split into its name and range.
func (p *Package) Relations() []Relation {
	out := []Relation{}

	if p.RPM != nil {
		for _, d := range p.RPM.Requires {
			if !strings.HasPrefix(d.Name, "rpmlib(") {
				out = append(out, Relation{Name: d.Name, Range: rpmRange(d)})
			}
		}

		return out
	}

	for _, d := range p.Depends {
		out = append(out, relationOf(d))
	}

	return out
}

// rpmRange is an rpm relation's range as written: "<= 1:2.0-3".
func rpmRange(d Dep) string {
	op, ok := rpmOps[d.Flags]
	if !ok {
		return ""
	}

	v := d.Ver
	if d.Epoch != "" && d.Epoch != "0" {
		v = d.Epoch + ":" + v
	}

	if d.Rel != "" {
		v += "-" + d.Rel
	}

	return op + " " + v
}

// relationOf splits "name (>= 1.0)", as apt writes it, or "name>=1.0", as
// pacman and apk do.
func relationOf(d string) Relation {
	d = strings.TrimSpace(d)

	// Alternatives stay whole: no one range is theirs.
	if strings.Contains(d, "|") {
		return Relation{Name: d}
	}

	if name, rest, ok := strings.Cut(d, "("); ok {
		return Relation{Name: strings.TrimSpace(name), Range: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), ")"))}
	}

	if i := strings.IndexAny(d, "<>=~"); i > 0 {
		return Relation{Name: d[:i], Range: d[i:]}
	}

	return Relation{Name: d}
}

// VersionOf is the answer for a version given its packages, each
// architecture's, the first one first.
func VersionOf(pkgs []*Package) Version {
	first := pkgs[0]
	v := Version{
		Name: first.Name, Version: first.Version, Description: first.Description, License: first.License,
		Size: first.Size, Depends: first.Relations(), Files: first.Files,
	}

	for _, p := range pkgs {
		v.Archs = append(v.Archs, p.Arch)
	}

	if v.Files == nil {
		v.Files = []File{}
	}

	return v
}

// Marshal is p as its meta object holds it.
func Marshal(p *Package) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}

	return b, nil
}

// Unmarshal reads a meta object.
func Unmarshal(b []byte) (*Package, error) {
	var p Package
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}

	return &p, nil
}
