// Package repo keeps one store's packages and publishes its index. The
// packages and their meta objects in S3 are the whole truth: an index is
// built from them alone, written as a new generation, then named by one
// pointer object. Several instances may share a bucket; none waits for
// another, and a rebuild that raced another lists again and rebuilds until
// the pointer names what the bucket holds.
package repo

import (
	"context"
	"crypto/md5" //nolint:gosec // apt and pacman list an MD5 sum
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // apt lists a SHA-1 sum
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sideways-works/piperepo/internal/format"
	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/store"
)

// Where a repository's objects live in its bucket.
const (
	PackagesPrefix = "packages/"
	MetaPrefix     = "meta/"
	IndexPrefix    = "index/"
	PointerKey     = "pointer.json"
	treeName       = "tree.json"
)

// Errors an upload or a delete answers.
var (
	errNotYet   = errors.New("repo: not in the index yet; the next reconcile lists it")
	errChanging = errors.New("repo: the bucket kept changing while the index was built; the next reconcile publishes it")
	ErrConflict = errors.New("another file of that name is there")
	ErrTooLarge = errors.New("too large")
	ErrNotFound = errors.New("no such package")
)

// Repo is one store's repository.
type Repo struct {
	Store  store.Store
	Format format.Format
	// Overwrite takes a file again with other bytes.
	Overwrite bool
	// MaxSize is the largest package taken, in bytes; 0 takes any.
	MaxSize int64
	// Grace is how long a generation is kept once a newer one was made.
	Grace time.Duration
	// PointerTTL is how long a read pointer is trusted.
	PointerTTL time.Duration
	// TempDir holds uploads while they are read.
	TempDir string
	// AfterPackage, when set, runs once a package's object is written and
	// before its meta object and its index.
	AfterPackage func()
	Log          *slog.Logger

	metas sync.Map
	trees sync.Map
	ptr   atomic.Pointer[cachedPointer]
	gcAt  atomic.Int64
}

// Pointer names the generation readers are served.
type Pointer struct {
	Gen    string    `json:"gen"`
	Digest string    `json:"digest"`
	At     time.Time `json:"at"`
}

type cachedPointer struct {
	p    *Pointer
	read time.Time
}

// Tree is one generation: every path it serves, and the packages it lists.
type Tree struct {
	Gen      string            `json:"gen"`
	Digest   string            `json:"digest"`
	Built    time.Time         `json:"built"`
	Index    map[string]Object `json:"index"`
	Packages map[string]Object `json:"packages"`
	Entries  []meta.Entry      `json:"entries"`
}

// Object is one served file: an index file of the generation, or a package.
type Object struct {
	File   string `json:"file,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (r *Repo) log() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}

	return r.Log
}

func metaKey(file string) string { return MetaPrefix + file + ".json" }

// listing is the bucket's packages and the digest of their listing.
func (r *Repo) listing(ctx context.Context) ([]store.Info, string, error) {
	all, err := r.Store.List(ctx, PackagesPrefix)
	if err != nil {
		return nil, "", err
	}

	h := sha256.New()
	for _, i := range all {
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", i.Key, i.Size, i.ETag)
	}

	return all, hex.EncodeToString(h.Sum(nil)), nil
}

// hashed copies a body to a temporary file, hashing it, and answers the
// file and what it read.
func (r *Repo) hashed(body io.Reader) (*os.File, *meta.Package, error) {
	f, err := os.CreateTemp(r.TempDir, "piperepo-*")
	if err != nil {
		return nil, nil, fmt.Errorf("repo: %w", err)
	}

	_ = os.Remove(f.Name()) //nolint:errcheck // open still, gone when closed

	s256, s1, m5 := sha256.New(), sha1.New(), md5.New() //nolint:gosec // the indexes list them
	src := body

	if r.MaxSize > 0 {
		src = io.LimitReader(body, r.MaxSize+1)
	}

	n, err := io.Copy(io.MultiWriter(f, s256, s1, m5), src)
	if err != nil {
		_ = f.Close() //nolint:errcheck // a temporary file, gone when closed

		return nil, nil, fmt.Errorf("repo: %w", err)
	}

	if r.MaxSize > 0 && n > r.MaxSize {
		_ = f.Close() //nolint:errcheck // a temporary file, gone when closed

		return nil, nil, fmt.Errorf("%w: at most %d bytes", ErrTooLarge, r.MaxSize)
	}

	sum := func(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

	return f, &meta.Package{Size: n, SHA256: sum(s256), SHA1: sum(s1), MD5: sum(m5)}, nil
}

// read parses a package's bytes in f.
func (r *Repo) read(file string, f *os.File, sums *meta.Package) (*meta.Package, error) {
	p, err := r.Format.Parse(file, f, sums.Size)
	if err != nil {
		return nil, err
	}

	p.File, p.Size, p.SHA256, p.SHA1, p.MD5 = file, sums.Size, sums.SHA256, sums.SHA1, sums.MD5

	return p, nil
}

// Upload stores a package, then its meta object, then publishes an index
// that lists it.
func (r *Repo) Upload(ctx context.Context, file string, body io.Reader) (*meta.Package, error) {
	if err := meta.FileName(file); err != nil {
		return nil, err
	}

	if err := r.Format.Accept(file); err != nil {
		return nil, err
	}

	f, sums, err := r.hashed(body)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // a temporary file, gone when closed

	p, err := r.read(file, f, sums)
	if err != nil {
		return nil, err
	}

	if info, err := r.Store.Head(ctx, PackagesPrefix+file); err == nil {
		had, err := r.metaOf(ctx, info)
		if err != nil {
			return nil, err
		}

		if had.SHA256 == p.SHA256 {
			return had, r.until(ctx, had)
		}

		if !r.Overwrite {
			return nil, fmt.Errorf("%w: %s, with other bytes", ErrConflict, file)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}

	etag, err := r.Store.Put(ctx, PackagesPrefix+file, f, p.Size)
	if err != nil {
		return nil, err
	}

	if r.AfterPackage != nil {
		r.AfterPackage()
	}

	p.ETag, p.Uploaded = etag, time.Now().UTC()
	if err := r.putMeta(ctx, p); err != nil {
		return nil, err
	}

	r.log().Info("package stored", "file", file, "name", p.Name, "version", p.Version, "arch", p.Arch)

	return p, r.until(ctx, p)
}

func (r *Repo) putMeta(ctx context.Context, p *meta.Package) error {
	b, err := meta.Marshal(p)
	if err != nil {
		return err
	}

	if _, err := store.PutBytes(ctx, r.Store, metaKey(p.File), b); err != nil {
		return err
	}

	r.metas.Store(PackagesPrefix+p.File+"\x00"+p.ETag, p)

	return nil
}

// until publishes until the served index lists p as it is now.
func (r *Repo) until(ctx context.Context, p *meta.Package) error {
	for range 20 {
		if err := r.Ensure(ctx); err != nil {
			return err
		}

		t, err := r.tree(ctx, true)
		if err != nil {
			return err
		}

		if slices.ContainsFunc(t.Entries, func(e meta.Entry) bool { return e.File == p.File && e.SHA256 == p.SHA256 }) {
			return nil
		}
	}

	return fmt.Errorf("%w: %s", errNotYet, p.File)
}

// Delete takes a package out and publishes an index without it.
func (r *Repo) Delete(ctx context.Context, file string) error {
	if err := meta.FileName(file); err != nil {
		return err
	}

	if _, err := r.Store.Head(ctx, PackagesPrefix+file); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, file)
	} else if err != nil {
		return err
	}

	if err := r.Store.Delete(ctx, PackagesPrefix+file); err != nil {
		return err
	}

	if err := r.Store.Delete(ctx, metaKey(file)); err != nil {
		return err
	}

	r.log().Info("package deleted", "file", file)

	return r.Ensure(ctx)
}

// metaOf is a package's meta object: kept, read, or made again from the
// package itself when it is missing or of other bytes.
func (r *Repo) metaOf(ctx context.Context, info store.Info) (*meta.Package, error) {
	cacheKey := info.Key + "\x00" + info.ETag
	if v, ok := r.metas.Load(cacheKey); ok {
		return v.(*meta.Package), nil //nolint:forcetypeassert,errcheck // only packages are kept
	}

	file := strings.TrimPrefix(info.Key, PackagesPrefix)

	b, err := store.ReadAll(ctx, r.Store, metaKey(file))
	if err == nil {
		if p, err := meta.Unmarshal(b); err == nil && p.ETag == info.ETag {
			r.metas.Store(cacheKey, p)

			return p, nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	return r.remake(ctx, info, file)
}

// remake reads a package again and writes its meta object.
func (r *Repo) remake(ctx context.Context, info store.Info, file string) (*meta.Package, error) {
	body, _, err := r.Store.Get(ctx, info.Key)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	f, sums, err := r.hashed(body)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // a temporary file, gone when closed

	p, err := r.read(file, f, sums)
	if err != nil {
		return nil, fmt.Errorf("repo: %s: %w", file, err)
	}

	p.ETag, p.Uploaded = info.ETag, info.Modified.UTC()
	if err := r.putMeta(ctx, p); err != nil {
		return nil, err
	}

	r.log().Info("meta object made again from its package", "file", file)

	return p, nil
}

// Ensure publishes an index of what the bucket holds, unless the pointer
// names one already.
func (r *Repo) Ensure(ctx context.Context) error {
	p, err := r.pointer(ctx, true)
	if err != nil {
		return err
	}

	_, digest, err := r.listing(ctx)
	if err != nil {
		return err
	}

	if p != nil && p.Digest == digest {
		return nil
	}

	return r.Rebuild(ctx)
}

// Rebuild publishes an index of what the bucket holds now, again until the
// listing it built from is still the bucket's once its pointer is written.
func (r *Repo) Rebuild(ctx context.Context) error {
	for range 10 {
		all, digest, err := r.listing(ctx)
		if err != nil {
			return err
		}

		pkgs, err := r.metasOf(ctx, all)
		if err != nil {
			return err
		}

		t, err := r.publish(ctx, pkgs, digest)
		if err != nil {
			return err
		}

		if _, now, err := r.listing(ctx); err != nil || now != digest {
			continue
		}

		ptr := Pointer{Gen: t.Gen, Digest: digest, At: time.Now().UTC()}

		b, err := json.Marshal(ptr)
		if err != nil {
			return fmt.Errorf("repo: %w", err)
		}

		if _, err := store.PutBytes(ctx, r.Store, PointerKey, b); err != nil {
			return err
		}

		r.ptr.Store(&cachedPointer{p: &ptr, read: time.Now()})
		r.log().Info("index published", "gen", t.Gen, "packages", len(pkgs))

		// A rebuild that listed less may have written its pointer over
		// this one's meanwhile: list again, and go on until it holds.
		if _, now, err := r.listing(ctx); err == nil && now == digest {
			r.collect(ctx, t.Gen)

			return nil
		}
	}

	return errChanging
}

// metasOf are the meta objects of a listing's packages, read at once.
func (r *Repo) metasOf(ctx context.Context, all []store.Info) ([]*meta.Package, error) {
	out := make([]*meta.Package, len(all))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)

	for i, info := range all {
		g.Go(func() error {
			p, err := r.metaOf(gctx, info)
			if err != nil {
				return err
			}

			out[i] = p

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}

	return out, nil
}

// newGen names a generation: its time first, so names sort by age.
func newGen(at time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return fmt.Sprintf("%020d-%s", at.UnixNano(), hex.EncodeToString(b))
}

func genTime(gen string) (time.Time, bool) {
	n, err := strconv.ParseInt(strings.SplitN(gen, "-", 2)[0], 10, 64)

	return time.Unix(0, n), err == nil
}

// publish writes a generation's index files, then its tree.
func (r *Repo) publish(ctx context.Context, pkgs []*meta.Package, digest string) (*Tree, error) {
	at := time.Now().UTC()

	files, err := r.Format.Index(pkgs, at)
	if err != nil {
		return nil, err
	}

	t := &Tree{Gen: newGen(at), Digest: digest, Built: at, Index: map[string]Object{}, Packages: map[string]Object{}}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)

	for path, data := range files {
		sum := sha256.Sum256(data)
		t.Index[path] = Object{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}

		g.Go(func() error {
			_, err := store.PutBytes(gctx, r.Store, IndexPrefix+t.Gen+"/"+path, data)

			return err
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}

	for _, p := range pkgs {
		paths := r.Format.Paths(p)
		for _, path := range paths {
			t.Packages[path] = Object{File: p.File, Size: p.Size, SHA256: p.SHA256}
		}

		t.Entries = append(t.Entries, meta.EntryOf(p, paths[0]))
	}

	sort.Slice(t.Entries, func(i, j int) bool {
		a, b := t.Entries[i], t.Entries[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}

		return a.File < b.File
	})

	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}

	if _, err := store.PutBytes(ctx, r.Store, IndexPrefix+t.Gen+"/"+treeName, b); err != nil {
		return nil, err
	}

	r.trees.Store(t.Gen, t)

	return t, nil
}

// pointer is the pointer, read anew when fresh or older than its TTL; nil
// before any index was published.
func (r *Repo) pointer(ctx context.Context, fresh bool) (*Pointer, error) {
	if c := r.ptr.Load(); c != nil && !fresh && time.Since(c.read) < r.PointerTTL {
		return c.p, nil
	}

	b, err := store.ReadAll(ctx, r.Store, PointerKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil //nolint:nilnil // no index yet
	}

	if err != nil {
		return nil, err
	}

	var p Pointer
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("repo: the pointer: %w", err)
	}

	r.ptr.Store(&cachedPointer{p: &p, read: time.Now()})

	return &p, nil
}

// ErrNoIndex is a repository nothing was published to yet.
var ErrNoIndex = errors.New("no index published yet")

// tree is the generation the pointer names.
func (r *Repo) tree(ctx context.Context, fresh bool) (*Tree, error) {
	p, err := r.pointer(ctx, fresh)
	if err != nil {
		return nil, err
	}

	if p == nil {
		return nil, ErrNoIndex
	}

	if v, ok := r.trees.Load(p.Gen); ok {
		return v.(*Tree), nil //nolint:forcetypeassert,errcheck // only trees are kept
	}

	b, err := store.ReadAll(ctx, r.Store, IndexPrefix+p.Gen+"/"+treeName)
	if err != nil {
		return nil, err
	}

	var t Tree
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("repo: tree %s: %w", p.Gen, err)
	}

	// Only the generation served and the one before stay in memory.
	r.trees.Range(func(k, _ any) bool {
		if k.(string) < p.Gen { //nolint:forcetypeassert,errcheck // only names are keys
			r.trees.Delete(k)
		}

		return true
	})
	r.trees.Store(p.Gen, &t)

	return &t, nil
}

// Tree is the generation served now.
func (r *Repo) Tree(ctx context.Context) (*Tree, error) { return r.tree(ctx, false) }

// Meta is a listed package's meta object, its files with it.
func (r *Repo) Meta(ctx context.Context, file string) (*meta.Package, error) {
	b, err := store.ReadAll(ctx, r.Store, metaKey(file))
	if err != nil {
		return nil, err
	}

	return meta.Unmarshal(b)
}

// Reconcile publishes when the pointer doesn't name what the bucket holds:
// an upload whose instance died before its index, or a race lost.
func (r *Repo) Reconcile(ctx context.Context) error {
	if err := r.Ensure(ctx); err != nil {
		return err
	}

	if p, err := r.pointer(ctx, false); err == nil && p != nil {
		r.collect(ctx, p.Gen)
	}

	return nil
}

// collect deletes generations a newer one replaced longer than the grace
// ago, at most once a minute; the one served stays.
func (r *Repo) collect(ctx context.Context, current string) {
	last := r.gcAt.Load()
	if time.Since(time.Unix(0, last)) < time.Minute || !r.gcAt.CompareAndSwap(last, time.Now().UnixNano()) {
		return
	}

	gens, err := r.Store.Prefixes(ctx, IndexPrefix)
	if err != nil {
		r.log().Warn("generations not listed", "err", err)

		return
	}

	slices.Sort(gens)

	for i := 0; i+1 < len(gens); i++ {
		gen := strings.TrimSuffix(strings.TrimPrefix(gens[i], IndexPrefix), "/")

		next, ok := genTime(strings.TrimSuffix(strings.TrimPrefix(gens[i+1], IndexPrefix), "/"))
		if !ok || gen == current || time.Since(next) < r.Grace {
			continue
		}

		objs, err := r.Store.List(ctx, gens[i])
		if err != nil {
			continue
		}

		for _, o := range objs {
			if err := r.Store.Delete(ctx, o.Key); err != nil {
				r.log().Warn("generation not deleted whole", "gen", gen, "err", err)

				break
			}
		}

		r.trees.Delete(gen)
	}
}

// Open is a served path's body: an index file of the generation served, or
// a package; its object says what it is.
func (r *Repo) Open(ctx context.Context, path string) (io.ReadCloser, Object, bool, error) {
	t, err := r.Tree(ctx)
	if err != nil {
		return nil, Object{}, false, err
	}

	if o, ok := t.Index[path]; ok {
		body, _, err := r.Store.Get(ctx, IndexPrefix+t.Gen+"/"+path)

		return body, o, true, err
	}

	if o, ok := t.Packages[path]; ok {
		body, _, err := r.Store.Get(ctx, PackagesPrefix+o.File)

		return body, o, false, err
	}

	return nil, Object{}, false, store.ErrNotFound
}
