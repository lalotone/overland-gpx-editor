// Command overland serves and manages the local GPX track library.
package main

import (
	"context"
	"fmt"
	"os"

	importcmd "github.com/lalotone/overland-gpx-editor/cmd/overland/import"
	"github.com/lalotone/overland-gpx-editor/cmd/overland/serve"
	"github.com/lalotone/overland-gpx-editor/cmd/overland/user"
	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/urfave/cli/v3"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newCommand().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", util.AppName, err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:    util.AppName,
		Usage:   "Plan, edit, serve, and import GPX tracks",
		Version: version,
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "port",
				Usage: "serve on 127.0.0.1:`PORT` instead of --addr; for process managers that pass the port before the command",
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			return cli.ShowRootCommandHelp(cmd)
		},
		Commands: []*cli.Command{
			serve.Command,
			importcmd.Command,
			user.Command,
		},
	}
}
