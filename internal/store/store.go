// Package store keeps objects in S3: packages, their meta objects, index
// generations and the pointer. Memory is a store of its own for tests.
package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound is an object that isn't there.
var ErrNotFound = errors.New("not found")

// Info is what a listing says of an object.
type Info struct {
	Key      string
	Size     int64
	ETag     string
	Modified time.Time
}

// Store is where a repository's objects live.
type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, Info, error)
	// Put writes size bytes of body and answers the object's ETag.
	Put(ctx context.Context, key string, body io.Reader, size int64) (string, error)
	Head(ctx context.Context, key string) (Info, error)
	// List are every object under prefix, by key.
	List(ctx context.Context, prefix string) ([]Info, error)
	// Prefixes are the names right under prefix that end in "/".
	Prefixes(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// ReadAll is an object's whole body.
func ReadAll(ctx context.Context, s Store, key string) ([]byte, error) {
	r, _, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err //nolint:wrapcheck // the store's own
	}

	return b, nil
}

// PutBytes writes b.
func PutBytes(ctx context.Context, s Store, key string, b []byte) (string, error) {
	return s.Put(ctx, key, bytes.NewReader(b), int64(len(b)))
}

// Memory is a store in memory.
type Memory struct {
	objects sync.Map
	n       atomic.Int64
}

type memObject struct {
	data []byte
	info Info
}

// NewMemory is an empty store.
func NewMemory() *Memory { return &Memory{} }

func (m *Memory) Get(_ context.Context, key string) (io.ReadCloser, Info, error) {
	v, ok := m.objects.Load(key)
	if !ok {
		return nil, Info{}, ErrNotFound
	}

	o := v.(memObject) //nolint:forcetypeassert,errcheck // only objects are stored

	return io.NopCloser(bytes.NewReader(o.data)), o.info, nil
}

func (m *Memory) Put(_ context.Context, key string, body io.Reader, _ int64) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err //nolint:wrapcheck // the reader's own
	}

	etag := key + "#" + strconv.FormatInt(m.n.Add(1), 10)
	m.objects.Store(key, memObject{data: data, info: Info{Key: key, Size: int64(len(data)), ETag: etag, Modified: time.Now()}})

	return etag, nil
}

func (m *Memory) Head(ctx context.Context, key string) (Info, error) {
	r, info, err := m.Get(ctx, key)
	if err != nil {
		return Info{}, err
	}

	_ = r.Close()

	return info, nil
}

func (m *Memory) List(_ context.Context, prefix string) ([]Info, error) {
	var out []Info

	m.objects.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), prefix) { //nolint:forcetypeassert,errcheck // only strings are keys
			out = append(out, v.(memObject).info) //nolint:forcetypeassert,errcheck // only objects are stored
		}

		return true
	})

	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Key, b.Key) })

	return out, nil
}

func (m *Memory) Prefixes(ctx context.Context, prefix string) ([]string, error) {
	all, _ := m.List(ctx, prefix) //nolint:errcheck // never fails

	var out []string

	for _, i := range all {
		rest := strings.TrimPrefix(i.Key, prefix)
		if name, _, ok := strings.Cut(rest, "/"); ok && !slices.Contains(out, prefix+name+"/") {
			out = append(out, prefix+name+"/")
		}
	}

	return out, nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.objects.Delete(key)

	return nil
}
