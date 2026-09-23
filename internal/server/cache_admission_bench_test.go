package server

import (
	"fmt"
	"testing"
	"time"
)

// Regional writes and pin metadata updates must not scan the whole cache when
// there is quota available and no entry has reached its retention deadline.
func BenchmarkRegionalCacheAdmission(b *testing.B) {
	now := time.Now()
	s := &cacheStore{entries: make(map[string]*cacheMetadata), now: func() time.Time { return now }, maxBytes: 4 << 30, maxEntries: 200000}
	for i := range 40000 {
		meta := testMetadata(fmt.Sprintf("%064x", i), "maps-openfreemap", now)
		s.setEntryLocked(&meta)
	}
	b.ResetTimer()
	for b.Loop() {
		if err := s.admitLocked("maps-openfreemap", "", 10000); err != nil {
			b.Fatal(err)
		}
	}
}
