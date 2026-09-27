package server

import (
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// Generous enough for a long recorded track with per-point extensions, small
// enough that an unauthenticated PUT cannot fill the disk in one request.
const maxUploadBytes = 64 << 20

var errBadFilename = errors.New("invalid filename")

// safeGPXFilename accepts one bare GPX filename. os.Root is the filesystem
// boundary; this validation keeps the HTTP API deliberately narrower.
//
// Without this, a name such as ../../etc/passwd is read (and, on the delete
// route, unlinked) straight off the host filesystem.
func safeGPXFilename(filename string) (string, error) {
	if !strings.HasSuffix(strings.ToLower(filename), ".gpx") {
		return "", errors.New("only .gpx files are allowed")
	}
	// Reject anything with a directory component rather than stripping it:
	// silently rewriting a path the caller asked for is its own surprise.
	if strings.HasPrefix(filename, ".") || strings.ContainsAny(filename, `/\`) || strings.ContainsRune(filename, 0) {
		return "", errBadFilename
	}
	if filename != filepath.Base(filename) || filename == "." || filename == ".." {
		return "", errBadFilename
	}
	return filename, nil
}

// ValidateGPXFilename reports whether filename can be addressed through the
// track library API.
func ValidateGPXFilename(filename string) error {
	_, err := safeGPXFilename(filename)
	return err
}

// library is the track store. A track is identified by (owner, filename):
// every operation takes both, and handlers never build a path themselves, so
// sharing a track between owners can later be added here without touching
// them or the layout on disk. Each owner's tracks live under their own
// os.Root, so a filename can only ever name that owner's file.
type library struct {
	spaces *ownerSpaces
	mu     sync.RWMutex
}

// tracks opens the owner's track directory. Callers close it.
func (l *library) tracks(owner Owner) (*os.Root, error) {
	return l.spaces.openSub(owner, ownerTracksDir)
}

func (l *library) list(owner Owner) ([]string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	root, err := l.tracks(owner)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		// Skip dotfiles so a crashed save's leftover .tmp-*.gpx never shows
		// up in the library.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := root.Lstat(e.Name())
		if err == nil && info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".gpx") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// read returns a track's content. A missing file, an inaccessible one and a
// symlink that tried to escape all come back as os.ErrNotExist: the API does
// not reveal which.
func (l *library) read(owner Owner, filename string) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	root, err := l.tracks(owner)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(filename)
	if err != nil {
		return nil, os.ErrNotExist
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	return io.ReadAll(file)
}

// save replaces a track, creating it if needed.
func (l *library) save(owner Owner, filename string, content []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	root, err := l.tracks(owner)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeFileAtomic(root, filename, content)
}

// create publishes a new track and returns os.ErrExist when the owner
// already has one by that name.
func (l *library) create(owner Owner, filename string, content []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	root, err := l.tracks(owner)
	if err != nil {
		return err
	}
	defer root.Close()
	return createFile(root, filename, content)
}

// exists reports whether the owner has a track by that name.
func (l *library) exists(owner Owner, filename string) (bool, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	root, err := l.tracks(owner)
	if err != nil {
		return false, err
	}
	defer root.Close()
	_, err = root.Lstat(filename)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (l *library) remove(owner Owner, filename string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	root, err := l.tracks(owner)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(filename)
}

// resolveFile pulls {filename} off the request and validates it, writing the
// error response itself when the name is unusable.
func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) (string, bool) {
	filename, err := safeGPXFilename(chi.URLParam(r, "filename"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return filename, true
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	owner, _ := ownerOf(r)
	names, err := s.library.list(owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"files": names})
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	filename, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	owner, _ := ownerOf(r)
	content, err := s.library.read(owner, filename)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "File not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write(content)
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	filename, ok := s.resolveFile(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "Empty request body")
		return
	}
	owner, _ := ownerOf(r)
	if err := s.library.save(owner, filename, body); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "File saved successfully",
		"filename": filename,
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "Missing 'file' upload")
		return
	}
	defer file.Close()

	// The browser sends the name it read off disk; treat it as hostile input.
	filename, err := safeGPXFilename(filepath.Base(header.Filename))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	owner, _ := ownerOf(r)
	// The 409 only ever reflects the caller's own library: another owner's
	// files are under a different root and cannot collide.
	exists, err := s.library.exists(owner, filename)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists {
		writeError(w, http.StatusConflict, "File already exists")
		return
	}

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The early check avoids loading a duplicate into application memory. The
	// atomic publish below closes the race between same-name requests.
	if err := s.library.create(owner, filename, content); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusConflict, "File already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "File uploaded successfully",
		"filename": filename,
	})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	filename, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	owner, _ := ownerOf(r)
	if err := s.library.remove(owner, filename); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "File not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "File deleted successfully",
		"filename": filename,
	})
}

// writeFileAtomic replaces filename in one step, so an interrupted save cannot
// leave a half-written track in the library.
func writeFileAtomic(root *os.Root, filename string, content []byte) error {
	tmpName, err := stageFile(root, content)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	return root.Rename(tmpName, filename)
}

// createFile publishes a completed temporary file without replacing filename.
// Link is the portable standard-library create-if-absent primitive. Filesystems
// without hard links fall back to an exclusive direct write; the server's file
// lock keeps that fallback invisible to its readers until the write completes.
func createFile(root *os.Root, filename string, content []byte) error {
	tmpName, err := stageFile(root, content)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	if err := root.Link(tmpName, filename); err == nil || errors.Is(err, os.ErrExist) {
		return err
	}

	return createFileExclusive(root, filename, content)
}

// Tracks are private data: readable by the server's user only.
const trackFileMode = 0o600

func stageFile(root *os.Root, content []byte) (string, error) {
	tmpName := ".tmp-" + rand.Text() + ".gpx"
	tmp, err := root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, trackFileMode)
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			tmp.Close()
			root.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	complete = true
	return tmpName, nil
}

func createFileExclusive(root *os.Root, filename string, content []byte) error {
	file, err := root.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, trackFileMode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			file.Close()
			root.Remove(filename)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
