package meta

import (
	"slices"
	"testing"
)

func TestRelationsSplitEachFormatsDependencies(t *testing.T) {
	deb := &Package{Depends: []string{"libc6 (>= 2.36)", "zlib1g", "a | b (<< 2)"}}
	if got, want := deb.Relations(), []Relation{{"libc6", ">= 2.36"}, {"zlib1g", ""}, {"a | b (<< 2)", ""}}; !slices.Equal(got, want) {
		t.Errorf("apt: %+v", got)
	}

	pac := &Package{Depends: []string{"glibc>=2.36", "bash", "so:libc.musl-x86_64.so.1", "musl~1.2"}}
	if got, want := pac.Relations(), []Relation{{"glibc", ">=2.36"}, {"bash", ""}, {"so:libc.musl-x86_64.so.1", ""}, {"musl", "~1.2"}}; !slices.Equal(got, want) {
		t.Errorf("pacman and apk: %+v", got)
	}

	rpm := &Package{RPM: &RPM{Requires: []Dep{
		{Name: "glibc", Flags: "GE", Epoch: "0", Ver: "2.36"}, {Name: "/bin/sh"},
		{Name: "rpmlib(CompressedFileNames)", Flags: "LE", Ver: "3.0.4", Rel: "1"}, {Name: "perl", Flags: "EQ", Epoch: "4", Ver: "5.40", Rel: "2"},
	}}}
	if got, want := rpm.Relations(), []Relation{{"glibc", ">= 2.36"}, {"/bin/sh", ""}, {"perl", "= 4:5.40-2"}}; !slices.Equal(got, want) {
		t.Errorf("rpm: %+v", got)
	}

	if got := (&Package{}).Relations(); got == nil || len(got) != 0 {
		t.Errorf("none: %#v", got)
	}
}

func TestAVersionListsEveryArchitectureAndTheFirstsFiles(t *testing.T) {
	v := VersionOf([]*Package{
		{Name: "demo", Version: "1.0", Arch: "aarch64", Size: 10, Files: []File{{Path: "/usr/bin/demo", Size: 5}}},
		{Name: "demo", Version: "1.0", Arch: "x86_64", Size: 11},
	})

	if v.Name != "demo" || !slices.Equal(v.Archs, []string{"aarch64", "x86_64"}) || v.Size != 10 || len(v.Files) != 1 || v.Depends == nil {
		t.Fatalf("%+v", v)
	}
}
