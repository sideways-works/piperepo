// Package meta is what piperepo knows of one package: read from the
// package once at upload and kept as a small object beside it, so an index
// is rebuilt from these alone.
package meta

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// Entry is a package as the API lists it.
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
	URL         string    `json:"url"`
	Depends     []string  `json:"depends"`
	Uploaded    time.Time `json:"uploaded"`
	Files       []File    `json:"files,omitempty"`
}

// EntryOf is p as the API lists it, served at path.
func EntryOf(p *Package, path string, files bool) Entry {
	e := Entry{
		Name: p.Name, Version: p.Version, Arch: p.Arch, File: p.File, Path: path, Size: p.Size, SHA256: p.SHA256,
		Description: p.Description, License: p.License, URL: p.URL, Depends: p.Depends, Uploaded: p.Uploaded,
	}
	if e.Depends == nil {
		e.Depends = []string{}
	}

	if files {
		e.Files = p.Files
		if e.Files == nil {
			e.Files = []File{}
		}
	}

	return e
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
