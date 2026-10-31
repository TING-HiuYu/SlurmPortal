package gateway

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

func insideRoot(root, value string) bool {
	return value == root || strings.HasPrefix(value, strings.TrimSuffix(root, "/")+"/")
}
func remotePath(s *session, input string) (string, error) {
	root := s.root
	if root == "" {
		root = s.home
	}
	root = path.Clean(root)
	if input == "" {
		input = root
	}
	if len(input) > 4096 || strings.ContainsAny(input, "\x00\r\n") {
		return "", errors.New("invalid path")
	}
	if !path.IsAbs(input) {
		input = strings.TrimSuffix(s.home, "/") + "/" + input
	}
	if !insideRoot(root, input) {
		return "", errors.New("outside shared file root")
	}
	// REALPATH is not guaranteed to resolve links on every SFTP server. Resolve
	// each component using the authenticated user's own LSTAT/READLINK access.
	pending := strings.Split(input, "/")
	current := "/"
	hops, steps := 0, 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		steps++
		if steps > 4096 {
			return "", errors.New("path resolution limit")
		}
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			current = path.Dir(current)
			continue
		}
		candidate := path.Join(current, part)
		info, e := s.files.Lstat(candidate)
		if e != nil {
			return "", e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > 40 {
				return "", errors.New("symbolic link resolution limit")
			}
			target, e := s.files.ReadLink(candidate)
			if e != nil {
				return "", e
			}
			if target == "" || len(target) > 4096 || strings.ContainsAny(target, "\x00\r\n") {
				return "", errors.New("invalid link target")
			}
			if path.IsAbs(target) {
				current = "/"
			}
			pending = append(strings.Split(target, "/"), pending...)
			continue
		}
		current = candidate
	}
	if !insideRoot(root, current) {
		return "", errors.New("outside shared file root")
	}
	resolved, e := s.files.RealPath(current)
	if e != nil {
		return "", e
	}
	resolved = path.Clean(resolved)
	if !insideRoot(root, resolved) {
		return "", errors.New("outside shared file root")
	}
	return resolved, nil
}

func cancelSession(ctx context.Context, s *session) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = s.client.Close()
		case <-stop:
		}
	}()
	return func() { close(stop); <-done }
}
func (g *Gateway) list(w http.ResponseWriter, r *http.Request, s *session) {
	var input struct {
		Path string `json:"path"`
	}
	if body(r, &input) != nil {
		fail(w, 400, "request_invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stop := cancelSession(ctx, s)
	defer stop()
	meta, closeMeta, e := g.metadata(ctx, s)
	if e != nil {
		fail(w, 502, "metadata_unavailable")
		return
	}
	defer closeMeta()
	resolved, e := remotePath(meta, input.Path)
	if e != nil {
		fail(w, 403, "path_unavailable")
		return
	}
	entries, e := meta.files.ReadDirContext(ctx, resolved)
	if e != nil {
		fail(w, 502, "directory_unavailable")
		return
	}
	truncated := len(entries) > 10000
	if truncated {
		entries = entries[:10000]
	}
	items := make([]map[string]any, 0, len(entries))
	for _, v := range entries {
		if v.Name() == "." || v.Name() == ".." {
			continue
		}
		isDir, size := v.IsDir(), v.Size()
		isLink := v.Mode()&os.ModeSymlink != 0
		if isLink {
			if target, err := remotePath(meta, path.Join(resolved, v.Name())); err == nil {
				if info, err := meta.files.Stat(target); err == nil {
					isDir, size = info.IsDir(), info.Size()
				}
			}
		}
		items = append(items, map[string]any{"name": v.Name(), "path": path.Join(resolved, v.Name()), "is_dir": isDir, "is_symlink": isLink, "size": size, "modified_at": v.ModTime().UTC().Format(time.RFC3339), "mode": v.Mode().String()})
	}
	reply(w, 200, map[string]any{"path": resolved, "home": s.home, "file_root": s.root, "entries": items, "truncated": truncated})
}

type downloadInput struct {
	Paths []string `json:"paths"`
}

func pathsValid(values []string) bool {
	if len(values) == 0 || len(values) > 128 {
		return false
	}
	for _, v := range values {
		if len(v) == 0 || len(v) > 4096 || strings.ContainsAny(v, "\x00\r\n") {
			return false
		}
	}
	return true
}
func (g *Gateway) downloadTicket(w http.ResponseWriter, r *http.Request, s *session) {
	var input downloadInput
	if body(r, &input) != nil || !pathsValid(input.Paths) {
		fail(w, 400, "request_invalid")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.downloads) >= 1024 {
		fail(w, 429, "download_limit")
		return
	}
	id := opaque()
	expires := time.Now().Add(time.Minute)
	g.downloads[id] = download{sessionID: s.id, owner: s.owner, paths: append([]string{}, input.Paths...), expires: expires}
	reply(w, 201, map[string]any{"url": "/v1/downloads/" + id, "expires_at": expires.UTC().Format(time.RFC3339)})
}
func (g *Gateway) downloadOnce(w http.ResponseWriter, r *http.Request) {
	claims, e := g.auth(r, "")
	if e != nil {
		fail(w, 403, "ticket_invalid")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/downloads/")
	g.mu.Lock()
	v, found := g.downloads[id]
	s := g.sessions[v.sessionID]
	if !found || !v.expires.After(time.Now()) || s == nil || s.owner != v.owner {
		g.mu.Unlock()
		fail(w, 404, "download_expired")
		return
	}
	if s.owner != claims.Subject || s.username != claims.Username || s.cluster != claims.ClusterID {
		g.mu.Unlock()
		fail(w, 403, "download_owner_denied")
		return
	}
	delete(g.downloads, id)
	s.refs.Add(1)
	s.last = time.Now()
	g.mu.Unlock()
	defer s.refs.Add(-1)
	g.streamDownload(w, r, s, v.paths)
}
func (g *Gateway) downloadDirect(w http.ResponseWriter, r *http.Request, s *session) {
	var input downloadInput
	if body(r, &input) != nil || !pathsValid(input.Paths) {
		fail(w, 400, "request_invalid")
		return
	}
	g.streamDownload(w, r, s, input.Paths)
}

type archiveEntry struct {
	remote, name string
	size         int64
	modified     time.Time
}

// Enumerate metadata only. File contents are never buffered in memory.
func (g *Gateway) enumerate(s *session, values []string) ([]archiveEntry, error) {
	var result []archiveEntry
	var total int64
	visited := map[string]bool{}
	walked := 0
	names := map[string]bool{}
	var walk func(string, string, int) error
	walk = func(remote, name string, depth int) error {
		walked++
		if depth > 64 || walked > 10000 {
			return errors.New("archive limit")
		}
		real, e := remotePath(s, remote)
		if e != nil {
			return e
		}
		info, e := s.files.Lstat(real)
		if e != nil {
			return e
		}
		if info.IsDir() {
			if visited[real] {
				return errors.New("duplicate directory")
			}
			visited[real] = true
			defer delete(visited, real)
			children, e := s.files.ReadDir(real)
			if e != nil {
				return e
			}
			for _, c := range children {
				if c.Name() == "." || c.Name() == ".." {
					continue
				}
				if e := walk(path.Join(real, c.Name()), path.Join(name, c.Name()), depth+1); e != nil {
					return e
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > g.cfg.MaxDownloadBytes-total {
			return errors.New("unsupported file or archive limit")
		}
		if name == "." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || names[name] {
			return errors.New("ambiguous archive name")
		}
		names[name] = true
		total += info.Size()
		result = append(result, archiveEntry{real, name, info.Size(), info.ModTime()})
		return nil
	}
	for _, v := range values {
		real, e := remotePath(s, v)
		if e != nil {
			return nil, e
		}
		if e := walk(real, path.Base(real), 0); e != nil {
			return nil, e
		}
	}
	return result, nil
}
func (g *Gateway) streamDownload(w http.ResponseWriter, r *http.Request, s *session, values []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 24*time.Hour)
	defer cancel()
	metadataCtx, metadataCancel := context.WithTimeout(ctx, 30*time.Second)
	meta, closeMeta, e := g.metadata(metadataCtx, s)
	if e != nil {
		metadataCancel()
		fail(w, 502, "metadata_unavailable")
		return
	}
	entries, e := g.enumerate(meta, values)
	single := false
	if e == nil && len(values) == 1 && len(entries) == 1 {
		real, _ := remotePath(meta, values[0])
		single = real == entries[0].remote
	}
	closeMeta()
	metadataCancel()
	if e != nil {
		fail(w, 403, "download_unavailable")
		return
	}
	// A canceled native download must close its own SFTP channel, not the
	// shared SSH transport used by subsequent browsing and download requests.
	// Unlike metadata, this channel streams file data without a byte budget.
	setupTimeout := time.AfterFunc(sshTimeout, cancel)
	files, closeFiles, e := fileChannel(ctx, s, 0)
	setupTimeout.Stop()
	if e != nil {
		fail(w, 502, "file_unavailable")
		return
	}
	defer closeFiles()
	if single {
		v := entries[0]
		f, e := files.Open(v.remote)
		if e != nil {
			fail(w, 502, "file_unavailable")
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": v.name}))
		http.ServeContent(w, r, v.name, v.modified, io.NewSectionReader(f, 0, v.size))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="download.zip"`)
	writer := zip.NewWriter(w)
	for _, v := range entries {
		f, e := files.Open(v.remote)
		if e != nil {
			return
		}
		header := &zip.FileHeader{Name: v.name, Method: zip.Store, Modified: v.modified}
		dst, e := writer.CreateHeader(header)
		if e == nil {
			_, e = io.CopyN(dst, f, v.size)
		}
		_ = f.Close()
		if e != nil {
			return
		}
	}
	_ = writer.Close()
}
