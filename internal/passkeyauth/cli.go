package passkeyauth

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

const userUsage = `Usage: %[1]s <command> [flags] [arguments]

Manage accounts. Accounts sign in with passkeys; the
server stores usernames and passkey public keys only.

Commands:
  add <username>              create an account and print a passkey enrollment link
  list                        list accounts
  show <username>             show an account and its passkeys
  update <username>           --rename NEW, --disable or --enable
  enroll <username>           print a new enrollment link (extra passkey or recovery)
  revoke <username> <id>      remove a passkey by ID prefix (see show); ends sessions
  logout <username>           end all sessions
  delete <username>           delete an account, its passkeys and sessions

Flags:
  --db PATH       account database (default: %[2]s)
  --origin URL    public URL of the server, for enrollment links (default: %[3]s)
  --ttl DURATION  enrollment link lifetime for add/enroll (default: 24h)
  --yes           delete without confirmation
`

type userOptions struct {
	db, origin, rename string
	ttl                time.Duration
	disable, enable    bool
	yes                bool
}

// parseInterspersed lets flags follow positional arguments, as in
// "myapp user add alice --ttl 1h".
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// CommandOptions configures Command for the host program.
type CommandOptions struct {
	Program       string // how users invoke it, e.g. "myapp user"
	DefaultDB     string // see DefaultStorePath
	DefaultOrigin string // e.g. http://localhost:8080
}

// Command implements account management: add, list, show, update, enroll,
// revoke, logout and delete. Wire it to a subcommand such as "myapp user".
func Command(args []string, stdin io.Reader, out io.Writer, opts CommandOptions) error {
	dbPath := opts.DefaultDB
	usage := fmt.Sprintf(userUsage, opts.Program, dbPath, opts.DefaultOrigin)
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = fmt.Fprint(out, usage)
		return nil
	}
	cmd := args[0]
	var o userOptions
	fs := flag.NewFlagSet(opts.Program+" "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.db, "db", dbPath, "")
	fs.StringVar(&o.origin, "origin", opts.DefaultOrigin, "")
	fs.DurationVar(&o.ttl, "ttl", 24*time.Hour, "")
	fs.StringVar(&o.rename, "rename", "", "")
	fs.BoolVar(&o.disable, "disable", false, "")
	fs.BoolVar(&o.enable, "enable", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	command, ok := userCommands[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if len(pos) != command.args {
		return fmt.Errorf("%s expects %d argument(s)\n\n%s", cmd, command.args, usage)
	}
	if err := o.validate(command.link); err != nil {
		return err
	}
	store, err := OpenStore(o.db)
	if err != nil {
		return err
	}
	defer store.Close()
	return command.run(cli{program: opts.Program, store: store, out: out, in: bufio.NewReader(stdin), opts: o}, pos)
}

// userCommands maps each subcommand to its positional argument count and
// whether it prints an enrollment link.
var userCommands = map[string]struct {
	args int
	link bool
	run  func(cli, []string) error
}{
	"add":    {1, true, func(c cli, p []string) error { return c.add(p[0]) }},
	"list":   {0, false, func(c cli, _ []string) error { return c.list() }},
	"show":   {1, false, func(c cli, p []string) error { return c.show(p[0]) }},
	"update": {1, false, func(c cli, p []string) error { return c.update(p[0]) }},
	"enroll": {1, true, func(c cli, p []string) error { return c.enroll(p[0]) }},
	"revoke": {2, false, func(c cli, p []string) error { return c.revoke(p[0], p[1]) }},
	"logout": {1, false, func(c cli, p []string) error { return c.logout(p[0]) }},
	"delete": {1, false, func(c cli, p []string) error { return c.delete(p[0]) }},
}

func (o userOptions) validate(link bool) error {
	if o.ttl <= 0 || o.ttl > 30*24*time.Hour {
		return errors.New("--ttl must be between 1s and 720h")
	}
	if link {
		_, err := ValidateOrigin(o.origin)
		return err
	}
	return nil
}

type cli struct {
	program string
	store   *Store
	out     io.Writer
	in      *bufio.Reader
	opts    userOptions
}

func (c cli) printf(format string, args ...any) { _, _ = fmt.Fprintf(c.out, format, args...) }

func (c cli) add(username string) error {
	if _, err := c.store.CreateUser(username); err != nil {
		return err
	}
	c.printf("Created %s.\n", username)
	return c.enroll(username)
}

func (c cli) revoke(username, id string) error {
	if err := c.store.DeleteCredential(username, id); err != nil {
		return err
	}
	// A passkey is revoked because it may be in the wrong hands, and the
	// sessions do not record which passkey opened them: end them all.
	n, err := c.store.DeleteSessions(username)
	if err != nil {
		return err
	}
	c.printf("Removed passkey %s from %s and ended %d session(s).\n", id, username, n)
	return nil
}

func (c cli) logout(username string) error {
	if _, err := c.store.UserByName(username); err != nil {
		return err
	}
	n, err := c.store.DeleteSessions(username)
	if err == nil {
		c.printf("Ended %d session(s) for %s.\n", n, username)
	}
	return err
}

func (c cli) enroll(username string) error {
	token, err := c.store.CreateEnrollment(username, c.opts.ttl)
	if err != nil {
		return err
	}
	c.printf("Open this link within %s to register a passkey (single use):\n\n  %s\n\n",
		c.opts.ttl, EnrollmentURL(c.opts.origin, token))
	c.printf("Share it privately; anyone with the link can add a passkey to %s.\n", username)
	return nil
}

func status(disabled bool) string {
	if disabled {
		return "disabled"
	}
	return "active"
}

func (c cli) list() error {
	users, err := c.store.ListUsers()
	if err != nil {
		return err
	}
	if len(users) == 0 {
		c.printf("No accounts. Create one with: %s add <username>\n", c.program)
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "USERNAME\tSTATUS\tPASSKEYS\tSESSIONS\tCREATED")
	for _, u := range users {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", u.Username, status(u.Disabled), u.Passkeys, u.Sessions, u.Created.Format(time.DateOnly))
	}
	return tw.Flush()
}

func (c cli) show(username string) error {
	a, err := c.store.UserByName(username)
	if err != nil {
		return err
	}
	creds, err := c.store.Credentials(a.ID)
	if err != nil {
		return err
	}
	c.printf("Username: %s\nStatus:   %s\nCreated:  %s\nPasskeys: %d\n", a.Username, status(a.Disabled), a.Created.Format(time.DateTime), len(creds))
	if len(creds) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\n  ID\tCREATED\tSYNCED")
	for _, cred := range creds {
		id := credentialID(cred.ID)
		if len(id) > 16 {
			id = id[:16]
		}
		synced := "no"
		if cred.Flags.BackupState {
			synced = "yes"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", id, cred.Created.Format(time.DateTime), synced)
	}
	return tw.Flush()
}

func (c cli) update(username string) error {
	o := c.opts
	if o.disable && o.enable {
		return errors.New("use either --disable or --enable")
	}
	if o.rename == "" && !o.disable && !o.enable {
		return errors.New("nothing to update: use --rename NEW, --disable or --enable")
	}
	if _, err := c.store.UserByName(username); err != nil {
		return err
	}
	if o.disable || o.enable {
		if err := c.store.SetDisabled(username, o.disable); err != nil {
			return err
		}
		c.printf("%s is now %s.\n", username, status(o.disable))
	}
	if o.rename != "" {
		if err := c.store.RenameUser(username, o.rename); err != nil {
			return err
		}
		c.printf("Renamed %s to %s.\n", username, o.rename)
	}
	return nil
}

func (c cli) delete(username string) error {
	a, err := c.store.UserByName(username)
	if err != nil {
		return err
	}
	if !c.opts.yes {
		c.printf("Delete %s with all passkeys and sessions? Type the username to confirm: ", a.Username)
		line, _ := c.in.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), a.Username) {
			return errors.New("not deleted")
		}
	}
	if err := c.store.DeleteUser(username); err != nil {
		return err
	}
	c.printf("Deleted %s.\n", a.Username)
	return nil
}
