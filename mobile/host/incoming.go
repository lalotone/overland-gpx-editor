package host

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/lalotone/overland-gpx-editor/internal/server"
)

// Native staging generates every path component; the original filename is only
// metadata. This API never opens a URI or accepts a caller-supplied local path.
var incomingID = regexp.MustCompile(`^[0-9]{13}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type incomingItem struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

func (h *Host) incomingGPX(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/mobile/incoming/")
	listing := r.URL.Path == "/mobile/incoming"
	if !listing && !incomingID.MatchString(id) {
		http.Error(w, "Invalid shared file ID", http.StatusBadRequest)
		return
	}
	if (listing && r.Method != http.MethodGet) || (!listing && r.Method != http.MethodGet && r.Method != http.MethodDelete) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.incoming.Lock()
	defer h.incoming.Unlock()
	root, err := h.root.OpenRoot("incoming")
	if errors.Is(err, os.ErrNotExist) && listing {
		reply(w, []incomingItem{})
		return
	}
	if err != nil {
		http.Error(w, "Shared file inbox is unavailable", http.StatusNotFound)
		return
	}
	defer root.Close()
	if listing {
		listIncoming(w, root)
		return
	}
	if r.Method == http.MethodDelete {
		if err := root.RemoveAll(id); err != nil {
			http.Error(w, "Cannot dismiss shared file", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	dir, err := root.OpenRoot(id)
	if err != nil {
		http.Error(w, "Shared file is unavailable", http.StatusNotFound)
		return
	}
	defer dir.Close()
	name, err := incomingFilename(dir)
	if err != nil {
		http.Error(w, "Invalid shared GPX filename", http.StatusBadRequest)
		return
	}
	data, err := readIncomingFile(dir, "document.gpx", maxDocument)
	if err != nil || len(data) == 0 {
		http.Error(w, "Shared GPX is unreadable, empty, or exceeds 16 MiB", http.StatusBadRequest)
		return
	}
	reply(w, Document{Filename: name, Content: string(data)})
}

func listIncoming(w http.ResponseWriter, root *os.Root) {
	dir, err := root.Open(".")
	if err != nil {
		http.Error(w, "Cannot list shared files", http.StatusInternalServerError)
		return
	}
	defer dir.Close()
	// Native intake caps pending files at eight. Keep a bound here as well.
	entries, err := dir.ReadDir(32)
	if err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Cannot list shared files", http.StatusInternalServerError)
		return
	}
	items := []incomingItem{}
	for _, entry := range entries {
		if !entry.IsDir() || !incomingID.MatchString(entry.Name()) {
			continue // Includes unfinished .tmp directories and symlinks.
		}
		name := "Unreadable shared GPX"
		if item, err := root.OpenRoot(entry.Name()); err == nil {
			if filename, err := incomingFilename(item); err == nil {
				name = filename
			}
			item.Close()
		}
		items = append(items, incomingItem{ID: entry.Name(), Filename: name})
	}
	// IDs start with the receiving timestamp, so oldest shares are offered first.
	slices.SortFunc(items, func(a, b incomingItem) int { return strings.Compare(a.ID, b.ID) })
	reply(w, items)
}

func incomingFilename(root *os.Root) (string, error) {
	data, err := readIncomingFile(root, "metadata.json", 1024)
	if err != nil {
		return "", err
	}
	var metadata struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	return metadata.Filename, server.ValidateGPXFilename(metadata.Filename)
}

func readIncomingFile(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid shared file size or type")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("shared file exceeds limit")
	}
	return data, err
}
