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
	if strings.ContainsAny(filename, `/\`) || strings.ContainsRune(filename, 0) {
		return "", errBadFilename
	}
	if filename != filepath.Base(filename) || filename == "." || filename == ".." {
		return "", errBadFilename
	}
	return filename, nil
}

// resolveFile pulls {filename} off the request and validates it, writing the
// error response itself when the name is unusable.
func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) (string, bool) {
	filename, err := safeGPXFilename(r.PathValue("filename"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return filename, true
}

func (s *Server) gpxFiles() ([]string, error) {
	s.gpxMu.RLock()
	defer s.gpxMu.RUnlock()

	dir, err := s.gpxRoot.Open(".")
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
		info, err := s.gpxRoot.Lstat(e.Name())
		if err == nil && info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".gpx") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	names, err := s.gpxFiles()
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
	s.gpxMu.RLock()
	file, err := s.gpxRoot.Open(filename)
	if err != nil {
		s.gpxMu.RUnlock()
		// Do not reveal whether a rooted open failed because the file is absent,
		// inaccessible, or a symlink tried to escape the library.
		writeError(w, http.StatusNotFound, "File not found")
		return
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		s.gpxMu.RUnlock()
		writeError(w, http.StatusNotFound, "File not found")
		return
	}
	content, err := io.ReadAll(file)
	file.Close()
	s.gpxMu.RUnlock()
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
	s.gpxMu.Lock()
	err = writeFileAtomic(s.gpxRoot, filename, body)
	s.gpxMu.Unlock()
	if err != nil {
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
	s.gpxMu.RLock()
	_, statErr := s.gpxRoot.Lstat(filename)
	s.gpxMu.RUnlock()
	if statErr == nil {
		writeError(w, http.StatusConflict, "File already exists")
		return
	} else if !errors.Is(statErr, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, statErr.Error())
		return
	}

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The early check avoids loading a duplicate into application memory. The
	// atomic publish below closes the race between same-name requests.
	s.gpxMu.Lock()
	err = createFile(s.gpxRoot, filename, content)
	s.gpxMu.Unlock()
	if err != nil {
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
	s.gpxMu.Lock()
	err := s.gpxRoot.Remove(filename)
	s.gpxMu.Unlock()
	if err != nil {
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

func stageFile(root *os.Root, content []byte) (string, error) {
	tmpName := ".tmp-" + rand.Text() + ".gpx"
	tmp, err := root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
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
	if err := tmp.Chmod(0o644); err != nil {
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
	file, err := root.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
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
	if err := file.Chmod(0o644); err != nil {
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
