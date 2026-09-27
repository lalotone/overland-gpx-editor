// Package user manages the passkey accounts used by serve --auth.
package user

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lalotone/overland-gpx-editor/cmd/overland/identity"
	"github.com/lalotone/overland-gpx-editor/cmd/overland/util"
	"github.com/lalotone/overland-gpx-editor/internal/passkeyauth"
	"github.com/lalotone/overland-gpx-editor/internal/server"
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
		args := cmd.Args().Slice()
		if len(args) > 0 && args[0] == "delete" {
			return deleteUser(args[1:], os.Stdin, cmd.Writer)
		}
		if err := passkeyauth.Command(args, os.Stdin, os.Stdout, Options()); err != nil {
			return err
		}
		if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
			fmt.Fprint(os.Stdout, deleteUsage)
		}
		return nil
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

const deleteUsage = `
delete also removes the account's private data: its track library, trip
packs and cached searches under --data-dir (default: $DATA_DIR or
` + "`" + `serve --data-dir` + "`" + `'s default). --keep-data leaves them on disk. Stop the
server first, or restart it afterwards: a running server keeps the
account's trip packs in memory until it reloads them.
`

// deleteUser is the one account command handled here rather than by
// passkeyauth: deleting an account must also delete what it owns, and only
// the CLI holds both the account database and the owner key needed to find
// that directory. The confirmation and database steps match passkeyauth's.
func deleteUser(args []string, stdin io.Reader, out io.Writer) error {
	opts := Options()
	fs := flag.NewFlagSet(opts.Program+" delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", opts.DefaultDB, "")
	dataDir := fs.String("data-dir", envOr("DATA_DIR", util.DefaultDataDir()), "")
	keepData := fs.Bool("keep-data", false, "")
	yes := fs.Bool("yes", false, "")
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return errors.New("delete expects 1 argument(s): the username")
	}
	username := positional[0]
	store, err := passkeyauth.OpenStore(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	account, err := store.UserByName(username)
	if err != nil {
		return err
	}
	// Resolve the owner before deleting the account: the handle is gone with it.
	var owner server.Owner
	if !*keepData {
		secret, err := identity.Load(identity.KeyPath(*db))
		if errors.Is(err, os.ErrNotExist) {
			// No key means serve --auth never ran here, so nothing was stored.
			*keepData = true
		} else if err != nil {
			return fmt.Errorf("owner key: %w", err)
		} else if owner, err = secret.Owner(account); err != nil {
			return err
		}
	}
	if !*yes {
		what := "with all passkeys, sessions, tracks and trip packs"
		if *keepData {
			what = "with all passkeys and sessions"
		}
		fmt.Fprintf(out, "Delete %s %s? Type the username to confirm: ", account.Username, what)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), account.Username) {
			return errors.New("not deleted")
		}
	}
	if err := store.DeleteUser(username); err != nil {
		return err
	}
	fmt.Fprintf(out, "Deleted %s.\n", account.Username)
	if *keepData {
		return nil
	}
	if err := server.RemoveOwner(*dataDir, owner); err != nil {
		return fmt.Errorf("account deleted, but removing its data failed: %w", err)
	}
	fmt.Fprintf(out, "Removed the account's tracks, trip packs and cached searches.\n")
	return nil
}
