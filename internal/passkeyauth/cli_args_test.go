package passkeyauth

import (
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestFlags() (*flag.FlagSet, *userOptions) {
	var o userOptions
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.StringVar(&o.db, "db", "", "")
	fs.DurationVar(&o.ttl, "ttl", time.Hour, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	return fs, &o
}

// Passkey IDs are base64url and can begin with '-'. This used to fail as an
// unknown flag whenever a random test ID did (TestRevokeEndsSessions, ~1/64).
func TestPositionalArgumentsMayStartWithADash(t *testing.T) {
	for _, args := range [][]string{
		{"alice", "-JPBScF4", "--db", "x.db"},
		{"--db", "x.db", "alice", "-JPBScF4"},
		{"alice", "--db=x.db", "-JPBScF4"},
		{"alice", "--", "-JPBScF4", "--db", "ignored"},
	} {
		fs, o := newTestFlags()
		pos, err := parseInterspersed(fs, args)
		require.NoError(t, err, args)
		assert.Equal(t, "alice", pos[0], args)
		assert.Equal(t, "-JPBScF4", pos[1], args)
		if args[1] != "--" {
			assert.Equal(t, "x.db", o.db, args)
		}
	}
}

func TestUnknownLongFlagIsStillAnError(t *testing.T) {
	fs, _ := newTestFlags()
	_, err := parseInterspersed(fs, []string{"alice", "--bogus"})
	assert.ErrorContains(t, err, "flag provided but not defined: --bogus")
	fs, _ = newTestFlags()
	pos, err := parseInterspersed(fs, []string{"alice", "--", "--JPBScF4"})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "--JPBScF4"}, pos, "-- lets an ID starting with -- through")
}

func TestDefinedFlagsStillParse(t *testing.T) {
	fs, o := newTestFlags()
	pos, err := parseInterspersed(fs, []string{"alice", "--ttl", "2h", "--yes", "-db", "a.db"})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice"}, pos)
	assert.Equal(t, 2*time.Hour, o.ttl)
	assert.True(t, o.yes)
	assert.Equal(t, "a.db", o.db)
	fs, _ = newTestFlags()
	_, err = parseInterspersed(fs, []string{"alice", "--ttl", "soon"})
	assert.Error(t, err, "a defined flag with a bad value is still an error")
}
