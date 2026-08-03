package server

import (
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

// safeGPXPath resolves filename inside dir, refusing anything that escapes it.
//
// Without this, a name such as ../../etc/passwd is read (and, on the delete
// route, unlinked) straight off the host filesystem.
func safeGPXPath(dir, filename string) (string, error) {
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
	return filepath.Join(dir, filename), nil
}

// resolveFile pulls {filename} off the request and validates it, writing the
// error response itself when the name is unusable.
func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) (string, bool) {
	path, err := safeGPXPath(s.gpxDir, r.PathValue("filename"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return path, true
}

func (s *Server) gpxFiles() ([]string, error) {
	entries, err := os.ReadDir(s.gpxDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		// Skip dotfiles so a crashed save's leftover .tmp-*.gpx never shows
		// up in the library.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".gpx") {
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
	path, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "File not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write(content)
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	path, ok := s.resolveFile(w, r)
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
	if err := writeFileAtomic(path, body); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "File saved successfully",
		"filename": filepath.Base(path),
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Missing 'file' upload")
		return
	}
	defer file.Close()

	// The browser sends the name it read off disk; treat it as hostile input.
	path, err := safeGPXPath(s.gpxDir, filepath.Base(header.Filename))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := writeFileAtomic(path, content); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "File uploaded successfully",
		"filename": filepath.Base(path),
	})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	path, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "File not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "File deleted successfully",
		"filename": filepath.Base(path),
	})
}

// writeFileAtomic replaces path in one step, so an interrupted save cannot
// leave a half-written track in the library.
func writeFileAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.gpx")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
