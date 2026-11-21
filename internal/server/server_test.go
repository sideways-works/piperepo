package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sideways-works/piperepo/internal/format"
	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/repo"
	"github.com/sideways-works/piperepo/internal/sign"
	"github.com/sideways-works/piperepo/internal/store"
	"github.com/sideways-works/piperepo/internal/testpkg"
)

const token = "the store's credential"

func start(t *testing.T) *httptest.Server {
	t.Helper()

	k, err := sign.OpenPGPFromSeed([]byte("the seed every test instance shares"), "test")
	if err != nil {
		t.Fatal(err)
	}

	f, err := format.New("apt", format.Options{}, format.Signer{PGP: k})
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &repo.Repo{Store: store.NewMemory(), Format: f, Grace: time.Hour, PointerTTL: time.Second, TempDir: t.TempDir(), Log: log}

	s, err := New(r, []byte(token), log)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path string, body []byte, auth func(*http.Request)) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if auth != nil {
		auth(req)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return resp, b
}

func basic(r *http.Request)  { r.SetBasicAuth("piperun", token) }
func bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
func wrong(r *http.Request)  { r.SetBasicAuth("piperun", "not it") }

func TestEverythingButHealthNeedsTheCredential(t *testing.T) {
	srv := start(t)

	if resp, _ := do(t, srv, http.MethodGet, "/health", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", resp.StatusCode)
	}

	for _, auth := range []func(*http.Request){nil, wrong} {
		for _, p := range []string{"/api/packages", "/key.asc", "/dists/stable/InRelease"} {
			if resp, _ := do(t, srv, http.MethodGet, p, nil, auth); resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: %d", p, resp.StatusCode)
			}
		}

		if resp, _ := do(t, srv, http.MethodPut, "/upload/demo_1.0_all.deb", testpkg.Deb("demo", "1.0", "all"), auth); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("an upload: %d", resp.StatusCode)
		}
	}

	if resp, _ := do(t, srv, http.MethodGet, "/key.asc", nil, bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("a bearer: %d", resp.StatusCode)
	}
}

func TestAnUploadIsListedAndItsTreeServed(t *testing.T) {
	srv := start(t)

	resp, body := do(t, srv, http.MethodPut, "/upload/demo_1.0_all.deb", testpkg.Deb("demo", "1.0", "all"), basic)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the upload: %d %s", resp.StatusCode, body)
	}

	if resp.Header.Get("Piperepo-Name") != "demo" || resp.Header.Get("Piperepo-Version") != "1.0" || resp.Header.Get("Piperepo-Arch") != "all" {
		t.Fatalf("the upload's headers: %v", resp.Header)
	}

	if resp, body := do(t, srv, http.MethodPut, "/upload/demo_1.0_all.deb", append(testpkg.Deb("demo", "1.0", "all"), '\n', '\n'), basic); resp.StatusCode != http.StatusConflict {
		t.Fatalf("other bytes: %d %s", resp.StatusCode, body)
	}

	if resp, _ := do(t, srv, http.MethodPut, "/upload/demo.rpm", []byte("x"), basic); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("another format: %d", resp.StatusCode)
	}

	_, body = do(t, srv, http.MethodGet, "/api/packages?name=demo", nil, basic)

	var list []meta.Entry
	if err := json.Unmarshal(body, &list); err != nil || len(list) != 1 || list[0].Path != "pool/main/d/demo/demo_1.0_all.deb" || list[0].License != "MIT" {
		t.Fatalf("the list: %s %v", body, err)
	}

	_, body = do(t, srv, http.MethodGet, "/api/packages/demo/1.0", nil, basic)

	var detail []meta.Entry
	if err := json.Unmarshal(body, &detail); err != nil || len(detail) != 1 || len(detail[0].Files) == 0 {
		t.Fatalf("the version: %s %v", body, err)
	}

	if resp, _ := do(t, srv, http.MethodGet, "/api/packages/demo/9.9", nil, basic); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a version not there: %d", resp.StatusCode)
	}

	resp, body = do(t, srv, http.MethodGet, "/dists/stable/InRelease", nil, basic)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "BEGIN PGP SIGNED MESSAGE") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("InRelease: %d %v", resp.StatusCode, resp.Header)
	}

	resp, body = do(t, srv, http.MethodGet, "/pool/main/d/demo/demo_1.0_all.deb", nil, basic)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, testpkg.Deb("demo", "1.0", "all")) || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("the package: %d %v", resp.StatusCode, resp.Header)
	}

	etag := resp.Header.Get("ETag")
	if resp, _ := do(t, srv, http.MethodGet, "/pool/main/d/demo/demo_1.0_all.deb", nil, func(r *http.Request) { basic(r); r.Header.Set("If-None-Match", etag) }); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("the package again: %d", resp.StatusCode)
	}

	if resp, _ := do(t, srv, http.MethodDelete, "/api/packages/demo_1.0_all.deb", nil, basic); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the delete: %d", resp.StatusCode)
	}

	if resp, _ := do(t, srv, http.MethodGet, "/pool/main/d/demo/demo_1.0_all.deb", nil, basic); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the deleted package: %d", resp.StatusCode)
	}
}

func TestAnEmptyStoreListsNothing(t *testing.T) {
	srv := start(t)

	if resp, body := do(t, srv, http.MethodGet, "/api/packages", nil, basic); resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("the list: %d %s", resp.StatusCode, body)
	}

	if resp, _ := do(t, srv, http.MethodGet, "/dists/stable/InRelease", nil, basic); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("InRelease before any upload: %d", resp.StatusCode)
	}
}
