// Package objstoretest provides an in-memory S3 server with HEAD, GET and conditional PUT.
package objstoretest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Object is a stored object.
type Object struct {
	Body []byte
	ETag string
	Meta http.Header
}

// Server is the fake S3 endpoint.
type Server struct {
	URL string
	// Verify, when set, rejects requests whose signature does not check out.
	Verify func(r *http.Request) bool
	// BeforePut runs before each PUT is evaluated (to inject a competing writer).
	BeforePut func(s *Server, path string)

	mu      sync.Mutex
	objects map[string]Object
	etag    int
	Puts    int
}

// New starts a fake server that is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{objects: map[string]Object{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Object returns the stored object at path ("/bucket/key").
func (s *Server) Object(path string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[path]
	return o, ok
}

// Set stores an object directly, bypassing conditions.
func (s *Server) Set(path string, body []byte, meta http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(path, body, meta)
}

func (s *Server) setLocked(path string, body []byte, meta http.Header) {
	s.etag++
	s.objects[path] = Object{body, `"` + strconv.Itoa(s.etag) + `"`, meta}
}

// ServeHTTP handles signed HEAD, GET and conditional PUT requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") && r.URL.Query().Get("X-Amz-Signature") == "" || s.Verify != nil && !s.Verify(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPut && s.BeforePut != nil {
		s.BeforePut(s, r.URL.Path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, exists := s.objects[r.URL.Path]
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for k, v := range obj.Meta {
			w.Header()[k] = v
		}
		w.Header().Set("ETag", obj.ETag)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj.Body)
		}
	case http.MethodPut:
		s.Puts++
		if m := r.Header.Get("If-Match"); m != "" && (!exists || m != obj.ETag) || r.Header.Get("If-None-Match") == "*" && exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		meta := http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Amz-Meta-") {
				meta[k] = v
			}
		}
		s.setLocked(r.URL.Path, body, meta)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
