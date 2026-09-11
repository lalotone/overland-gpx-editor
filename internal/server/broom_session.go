package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
)

type sessionBroomProfile struct {
	profile *broom.Profile
	dataset *broomDataset
	graph   string
	expires time.Time
}

func (s *broomRoutingService) reapSessions() {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			var expired []*broomDataset
			s.mu.Lock()
			for id, session := range s.sessions {
				if time.Now().After(session.expires) {
					delete(s.sessions, id)
					expired = append(expired, session.dataset)
				}
			}
			s.mu.Unlock()
			for _, dataset := range expired {
				_ = dataset.close()
			}
		}
	}
}

func (s *Server) handleSessionProfile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Source string `json:"source"`
	}
	if err := decodeJSONBody(w, r, 256<<10, &request); err != nil || len(request.Source) == 0 || len(request.Source) > 128<<10 {
		writeError(w, http.StatusBadRequest, "provide a BRF profile no larger than 128 KiB")
		return
	}
	service := s.broom
	select {
	case service.uploads <- struct{}{}:
		defer func() { <-service.uploads }()
	default:
		writeError(w, http.StatusTooManyRequests, "A profile is already being prepared")
		return
	}
	profile, warnings, err := broom.ParseProfile(strings.NewReader(request.Source), "session-motorcycle")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid BRF profile: "+err.Error())
		return
	}
	service.mu.Lock()
	for id, session := range service.sessions {
		if time.Now().After(session.expires) {
			delete(service.sessions, id)
			_ = session.dataset.close()
		}
	}
	current := service.current
	if current == nil {
		service.mu.Unlock()
		writeBroomError(w, errRoutingNotReady)
		return
	}
	if len(service.sessions) >= 8 {
		service.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "Too many session profiles; remove an uploaded profile first")
		return
	}
	current.acquire()
	service.mu.Unlock()
	defer current.release()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(service.ctx, cancel)
	defer stop()
	dir, err := os.MkdirTemp("", "overland-session-profile-*")
	if err != nil {
		writeBroomError(w, err)
		return
	}
	zero := 0.0
	router, err := broom.Open(current.router.Path(), broom.RouterOptions{Profiles: []*broom.Profile{profile}, MetricDir: dir, MetricCacheSize: 1, DisableCustomizeOnDemand: true, MaxUncustomizedDistance: &zero})
	if err != nil {
		os.RemoveAll(dir)
		writeBroomError(w, err)
		return
	}
	dataset := newBroomDataset(router, current.regionID, current.generationID, "Session profile")
	dataset.temporaryDir = dir
	if err := router.Customize(ctx, profile); err != nil {
		dataset.close()
		writeBroomError(w, err)
		return
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		dataset.close()
		writeBroomError(w, err)
		return
	}
	id := hex.EncodeToString(token[:])
	service.mu.Lock()
	if service.current != current || ctx.Err() != nil {
		service.mu.Unlock()
		dataset.close()
		writeError(w, http.StatusConflict, "Routing region changed; upload the profile again")
		return
	}
	service.sessions[id] = &sessionBroomProfile{profile: profile, dataset: dataset, graph: router.Path(), expires: time.Now().Add(2 * time.Hour)}
	service.mu.Unlock()
	messages := []string{}
	for _, warning := range warnings {
		messages = append(messages, warning.String())
	}
	noStoreJSON(w, http.StatusCreated, map[string]any{"id": id, "warnings": messages})
}

func (s *Server) handleReleaseSessionProfile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, 4096, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broom.mu.Lock()
	session := s.broom.sessions[request.ID]
	delete(s.broom.sessions, request.ID)
	s.broom.mu.Unlock()
	if session != nil {
		_ = session.dataset.close()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *broomRoutingService) closeSessions() error {
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[string]*sessionBroomProfile)
	s.mu.Unlock()
	var err error
	for _, session := range sessions {
		err = errors.Join(err, session.dataset.close())
	}
	return err
}
