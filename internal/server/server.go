// Package server is piperepo's HTTP API: uploads, the package listing and
// the published tree, all behind one credential but /health.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/sideways-works/piperepo/internal/format"
	"github.com/sideways-works/piperepo/internal/meta"
	"github.com/sideways-works/piperepo/internal/repo"
	"github.com/sideways-works/piperepo/internal/store"
)

// Server answers one repository's requests.
type Server struct {
	Repo  *repo.Repo
	Token []byte
	Log   *slog.Logger
	// keys are the public key files, by path.
	keys map[string][]byte
}

// New is a server of r, its public keys served beside the tree.
func New(r *repo.Repo, token []byte, log *slog.Logger) (*Server, error) {
	keys, err := r.Format.Keys()
	if err != nil {
		return nil, err
	}

	return &Server{Repo: r, Token: token, Log: log, keys: keys}, nil
}

// Handler routes every request.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.Handle("PUT /upload/{file}", s.auth(http.HandlerFunc(s.upload)))
	mux.Handle("GET /api/packages", s.auth(http.HandlerFunc(s.list)))
	mux.Handle("GET /api/packages/{name}/{version}", s.auth(http.HandlerFunc(s.version)))
	mux.Handle("DELETE /api/packages/{file}", s.auth(http.HandlerFunc(s.remove)))
	mux.Handle("GET /", s.auth(http.HandlerFunc(s.serve)))

	return mux
}

// auth lets a request through with the credential: as basic
// authentication's password, any user, or as a bearer.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if _, pass, ok := r.BasicAuth(); ok {
			got = pass
		} else if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			got = b
		}

		if got == "" || subtle.ConstantTimeCompare([]byte(got), s.Token) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="piperepo"`)
			problem(w, http.StatusUnauthorized, "a credential is needed")

			return
		}

		next.ServeHTTP(w, r)
	})
}

func problem(w http.ResponseWriter, status int, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": why}) //nolint:errcheck,errchkjson // the client went away
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck,errchkjson // the client went away
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")

	p, err := s.Repo.Upload(r.Context(), file, r.Body)

	switch {
	case errors.Is(err, repo.ErrConflict):
		problem(w, http.StatusConflict, err.Error())
	case errors.Is(err, repo.ErrTooLarge):
		problem(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, format.ErrFormat), errors.Is(err, meta.ErrInvalid):
		problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.Log.Error("upload failed", "file", file, "err", err)
		problem(w, http.StatusInternalServerError, "the upload failed: "+err.Error())
	default:
		w.Header().Set("Piperepo-Name", p.Name)
		w.Header().Set("Piperepo-Version", p.Version)
		w.Header().Set("Piperepo-Arch", p.Arch)
		writeJSON(w, http.StatusCreated, meta.EntryOf(p, s.Repo.Format.Paths(p)[0], false))
	}
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) (*repo.Tree, bool) {
	t, err := s.Repo.Tree(r.Context())
	if errors.Is(err, repo.ErrNoIndex) {
		return &repo.Tree{}, true
	}

	if err != nil {
		s.Log.Error("the index isn't readable", "err", err)
		problem(w, http.StatusServiceUnavailable, "the index isn't readable right now")

		return nil, false
	}

	return t, true
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tree(w, r)
	if !ok {
		return
	}

	name := r.URL.Query().Get("name")
	out := []meta.Entry{}

	for _, e := range t.Entries {
		if name == "" || e.Name == name {
			out = append(out, e)
		}
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tree(w, r)
	if !ok {
		return
	}

	name, version := r.PathValue("name"), r.PathValue("version")
	out := []meta.Entry{}

	for _, e := range t.Entries {
		if e.Name != name || e.Version != version {
			continue
		}

		p, err := s.Repo.Meta(r.Context(), e.File)
		if err != nil {
			s.Log.Warn("meta object not read", "file", e.File, "err", err)
			out = append(out, e)

			continue
		}

		out = append(out, meta.EntryOf(p, e.Path, true))
	}

	if len(out) == 0 {
		problem(w, http.StatusNotFound, "no "+name+" "+version+" here")

		return
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	err := s.Repo.Delete(r.Context(), r.PathValue("file"))

	switch {
	case errors.Is(err, repo.ErrNotFound):
		problem(w, http.StatusNotFound, err.Error())
	case errors.Is(err, meta.ErrInvalid):
		problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.Log.Error("delete failed", "err", err)
		problem(w, http.StatusInternalServerError, "the delete failed: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// serve answers a path of the published tree: the public key, an index
// file of the generation served, or a package.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

	if k, ok := s.keys[p]; ok {
		sum := sha256.Sum256(k)
		s.send(w, r, io.NopCloser(strings.NewReader(string(k))), repo.Object{Size: int64(len(k)), SHA256: hex.EncodeToString(sum[:])}, "public, max-age=300")

		return
	}

	body, o, index, err := s.Repo.Open(r.Context(), p)

	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, repo.ErrNoIndex):
		problem(w, http.StatusNotFound, "no "+r.URL.Path+" here")

		return
	case err != nil:
		s.Log.Error("not served", "path", p, "err", err)
		problem(w, http.StatusServiceUnavailable, "the store isn't readable right now")

		return
	}

	cache := "public, max-age=31536000, immutable"
	if index && !s.Repo.Format.Immutable(p) {
		cache = "no-cache"
	}

	s.send(w, r, body, o, cache)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, body io.ReadCloser, o repo.Object, cache string) {
	defer body.Close()

	etag := `"` + o.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cache)

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	w.Header().Set("Content-Type", contentType(r.URL.Path))
	w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))

	if r.Method == http.MethodHead {
		return
	}

	if _, err := io.Copy(w, body); err != nil {
		s.Log.Debug("body not sent whole", "path", r.URL.Path, "err", err)
	}
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".xml"):
		return "application/xml"
	case strings.HasSuffix(p, ".asc"), strings.HasSuffix(p, "Release"), strings.HasSuffix(p, "Packages"), strings.HasSuffix(p, ".pub"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
