// Package user manages the passkey accounts used by serve --auth.
package user

import (
	"context"
	"os"
	"strings"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
	"github.com/urfave/cli/v3"
)

const defaultOrigin = "http://localhost:8000"

var Command = &cli.Command{
	Name:      "user",
	Usage:     "Manage passkey accounts for serve --auth",
	ArgsUsage: "add|list|show|update|enroll|revoke|logout|delete [flags] [arguments]",
	// passkeyauth parses its own subcommands and flags, including --help.
	SkipFlagParsing: true,
	Action: func(_ context.Context, cmd *cli.Command) error {
		return passkeyauth.Command(cmd.Args().Slice(), os.Stdin, os.Stdout, Options())
	},
}

// Options mirrors serve's AUTH_DB and AUTH_ORIGIN, so enrollment links and
// the database match the server started from the same environment.
func Options() passkeyauth.CommandOptions {
	return passkeyauth.CommandOptions{
		Program:       util.AppName + " user",
		DefaultDB:     envOr("AUTH_DB", util.DefaultAuthDB()),
		DefaultOrigin: envOr("AUTH_ORIGIN", defaultOrigin),
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
