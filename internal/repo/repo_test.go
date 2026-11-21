package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sideways-works/piperepo/internal/format"
	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/sign"
	"github.com/sideways-works/piperepo/internal/store"
	"github.com/sideways-works/piperepo/internal/testpkg"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// instance is one piperepo instance's repository over s.
func instance(t *testing.T, s store.Store) *Repo {
	t.Helper()

	k, err := sign.OpenPGPFromSeed([]byte("the seed every test instance shares"), "test")
	if err != nil {
		t.Fatal(err)
	}

	f, err := format.New("apt", format.Options{}, format.Signer{PGP: k})
	if err != nil {
		t.Fatal(err)
	}

	return &Repo{Store: s, Format: f, Grace: time.Hour, PointerTTL: time.Second, TempDir: t.TempDir(), Log: quiet}
}

func upload(t *testing.T, r *Repo, name, version string) *meta.Package {
	t.Helper()

	p, err := r.Upload(t.Context(), testpkg.Name("apt", name, version, "all"), bytes.NewReader(testpkg.Deb(name, version, "all")))
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// served are the files the index served now lists, read fresh.
func served(t *testing.T, r *Repo) []string {
	t.Helper()

	tr, err := r.tree(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var out []string
	for _, e := range tr.Entries {
		out = append(out, e.File)
	}

	slices.Sort(out)

	return out
}

// packagesFile is the amd64 Packages file the index served now holds.
func packagesFile(t *testing.T, r *Repo) string {
	t.Helper()

	body, _, _, err := r.Open(t.Context(), "dists/stable/main/binary-amd64/Packages")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()

	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestAnUploadIsListedAndServed(t *testing.T) {
	r := instance(t, store.NewMemory())
	p := upload(t, r, "demo", "1.0")

	if p.Name != "demo" || p.Version != "1.0" || p.ETag == "" {
		t.Fatalf("uploaded %+v", p)
	}

	if got := served(t, r); !slices.Equal(got, []string{"demo_1.0_all.deb"}) {
		t.Fatalf("the index lists %v", got)
	}

	if pk := packagesFile(t, r); !strings.Contains(pk, "Package: demo\n") {
		t.Fatalf("Packages: %s", pk)
	}

	body, o, index, err := r.Open(t.Context(), "pool/main/d/demo/demo_1.0_all.deb")
	if err != nil || index || o.SHA256 != p.SHA256 {
		t.Fatalf("the package: %+v %v %v", o, index, err)
	}

	_ = body.Close()

	if _, _, _, err := r.Open(t.Context(), "pool/main/n/none/none.deb"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a path no one uploaded: %v", err)
	}
}

func TestTheSameBytesAgainAreTakenAndOthersRefused(t *testing.T) {
	r := instance(t, store.NewMemory())
	upload(t, r, "demo", "1.0")
	upload(t, r, "demo", "1.0")

	other := otherBytes()

	if _, err := r.Upload(t.Context(), "demo_1.0_all.deb", bytes.NewReader(other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("other bytes of the same name: %v", err)
	}

	if _, err := r.Upload(t.Context(), "demo.rpm", bytes.NewReader(other)); !errors.Is(err, format.ErrFormat) {
		t.Fatalf("an rpm to an apt store: %v", err)
	}

	if _, err := r.Upload(t.Context(), "../demo.deb", bytes.NewReader(other)); !errors.Is(err, meta.ErrInvalid) {
		t.Fatalf("a path for a file name: %v", err)
	}
}

func TestOverwriteTakesOtherBytes(t *testing.T) {
	r := instance(t, store.NewMemory())
	upload(t, r, "demo", "1.0")

	r.Overwrite = true

	p, err := r.Upload(t.Context(), "demo_1.0_all.deb", bytes.NewReader(otherBytes()))
	if err != nil {
		t.Fatal(err)
	}

	tr, _ := r.tree(t.Context(), true)
	if len(tr.Entries) != 1 || tr.Entries[0].SHA256 != p.SHA256 {
		t.Fatalf("the index lists %+v", tr.Entries)
	}
}

// otherBytes is demo 1.0 again with other bytes: padding after its last
// member, which ar readers skip.
func otherBytes() []byte {
	return append(testpkg.Deb("demo", "1.0", "all"), '\n', '\n')
}

func TestADeletedPackageLeavesTheIndex(t *testing.T) {
	r := instance(t, store.NewMemory())
	upload(t, r, "demo", "1.0")
	upload(t, r, "other", "2.0")

	if err := r.Delete(t.Context(), "demo_1.0_all.deb"); err != nil {
		t.Fatal(err)
	}

	if got := served(t, r); !slices.Equal(got, []string{"other_2.0_all.deb"}) {
		t.Fatalf("the index lists %v", got)
	}

	if err := r.Delete(t.Context(), "demo_1.0_all.deb"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting it again: %v", err)
	}
}

// Uploads spread over two instances of one bucket at once all end in the
// index, whichever rebuild wrote the pointer last.
func TestConcurrentUploadsOverTwoInstancesAreAllIndexed(t *testing.T) {
	s := store.NewMemory()
	a, b := instance(t, s), instance(t, s)

	const n = 16

	var wg sync.WaitGroup

	errs := make(chan error, n)

	for i := range n {
		r := a
		if i%2 == 1 {
			r = b
		}

		name := fmt.Sprintf("pkg%02d", i)

		wg.Go(func() {
			_, err := r.Upload(context.Background(), testpkg.Name("apt", name, "1.0", "all"), bytes.NewReader(testpkg.Deb(name, "1.0", "all")))
			errs <- err
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, r := range []*Repo{a, b} {
		if got := served(t, r); len(got) != n {
			t.Fatalf("the index lists %d of %d: %v", len(got), n, got)
		}

		if pk := packagesFile(t, r); strings.Count(pk, "Package: ") != n {
			t.Fatalf("Packages lists %d of %d", strings.Count(pk, "Package: "), n)
		}
	}
}

// errDied stands for an instance's process ending.
var errDied = errors.New("the instance died")

// An instance that dies once a package is written, before its meta object
// and its index, leaves it to another instance's reconcile, which lists it.
func TestAnotherInstanceFinishesAnUploadItsInstanceDiedIn(t *testing.T) {
	s := store.NewMemory()
	a, b := instance(t, s), instance(t, s)
	upload(t, b, "before", "1.0")

	a.AfterPackage = func() { panic(errDied) }

	func() {
		defer func() {
			if v := recover(); v != errDied { //nolint:errorlint // the value panicked with
				t.Fatalf("the upload ended with %v", v)
			}
		}()

		_, _ = a.Upload(t.Context(), "lost_1.0_all.deb", bytes.NewReader(testpkg.Deb("lost", "1.0", "all"))) //nolint:errcheck // it panics
	}()

	if _, err := s.Head(t.Context(), metaKey("lost_1.0_all.deb")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a meta object was written: %v", err)
	}

	if got := served(t, b); slices.Contains(got, "lost_1.0_all.deb") {
		t.Fatal("indexed before any reconcile")
	}

	if err := b.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := served(t, b); !slices.Equal(got, []string{"before_1.0_all.deb", "lost_1.0_all.deb"}) {
		t.Fatalf("after the reconcile the index lists %v", got)
	}

	if _, err := s.Head(t.Context(), metaKey("lost_1.0_all.deb")); err != nil {
		t.Fatalf("its meta object: %v", err)
	}
}

// With every meta object gone, a new instance rebuilds the whole index
// from the packages in the bucket alone.
func TestEveryMetaObjectLostStillRebuildsFromTheBucket(t *testing.T) {
	s := store.NewMemory()
	a := instance(t, s)

	for _, n := range []string{"one", "two", "three"} {
		upload(t, a, n, "1.0")
	}

	before := packagesFile(t, a)

	metas, _ := s.List(t.Context(), MetaPrefix)
	for _, m := range metas {
		_ = s.Delete(t.Context(), m.Key)
	}

	_ = s.Delete(t.Context(), PointerKey)

	b := instance(t, s)
	if err := b.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := served(t, b); len(got) != 3 {
		t.Fatalf("the rebuilt index lists %v", got)
	}

	if after := packagesFile(t, b); after != before {
		t.Fatalf("the rebuilt Packages differs:\n%s\n---\n%s", before, after)
	}

	if again, _ := s.List(t.Context(), MetaPrefix); len(again) != 3 {
		t.Fatalf("%d meta objects made again", len(again))
	}
}

func TestReplacedGenerationsGoAfterTheGrace(t *testing.T) {
	s := store.NewMemory()
	r := instance(t, s)
	r.Grace = time.Nanosecond

	for _, n := range []string{"one", "two", "three"} {
		upload(t, r, n, "1.0")
		r.gcAt.Store(0)
		time.Sleep(time.Millisecond)
	}

	r.gcAt.Store(0)

	p, _ := r.pointer(t.Context(), true)
	r.collect(t.Context(), p.Gen)

	gens, _ := s.Prefixes(t.Context(), IndexPrefix)
	if len(gens) != 1 || gens[0] != IndexPrefix+p.Gen+"/" {
		t.Fatalf("generations left: %v, served %s", gens, p.Gen)
	}

	if pk := packagesFile(t, r); strings.Count(pk, "Package: ") != 3 {
		t.Fatal("the served generation went")
	}
}
