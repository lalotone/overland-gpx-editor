package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	broom "code.rbel.co/rubiojr/broom/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroomGraphErrors(t *testing.T) {
	// A path mentioning profiles must not turn damaged server data into a
	// client-side invalid-profile error through the legacy text fallback.
	path := filepath.Join(t.TempDir(), "profile-graph.broom")
	require.NoError(t, os.WriteFile(path, []byte("broken graph"), 0600))
	_, invalid := broom.Open(path, broom.RouterOptions{})
	require.ErrorIs(t, invalid, broom.ErrInvalidGraph)
	for _, tt := range []struct {
		name string
		err  error
		code string
	}{
		{"direct graph", invalid, "routing_data_invalid"},
		{"previous graph", fmt.Errorf("open previous routing graph: %w", invalid), "routing_data_invalid"},
		{"managed graph", fmt.Errorf("%w: %w", broom.ErrCatalogue, invalid), "routing_data_invalid"},
		{"unsupported host", fmt.Errorf("open profile graph: %w", broom.ErrUnsupportedEndian), "routing_platform_unsupported"},
		{"managed unsupported host", fmt.Errorf("%w: %w", broom.ErrCatalogue, broom.ErrUnsupportedEndian), "routing_platform_unsupported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeBroomError(response, tt.err)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			var body map[string]string
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, tt.code, body["code"])
			assert.Equal(t, tt.err.Error(), body["detail"])
		})
	}
}
