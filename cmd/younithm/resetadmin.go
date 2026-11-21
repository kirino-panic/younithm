package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"younithm/internal/auth"
	"younithm/internal/db"
)

// runResetAdmin implements `younithm reset-admin`. It operates directly on the
// local database, so filesystem access to the config dir is the credential.
//
// The new password is read from stdin (first line), never from argv, so it
// does not show up in `ps` or shell history:
//
//	echo 'new password here' | younithm reset-admin --stdin
//	younithm reset-admin            # prompts (input is visible; prefer --stdin)
func runResetAdmin(configDir string, args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("reset-admin", flag.ContinueOnError)
	fs.SetOutput(out)
	fromStdin := fs.Bool("stdin", false, "read the new password from the first line of stdin without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (passwords are not accepted on the command line)", fs.Arg(0))
	}

	br := bufio.NewReader(stdin)
	if !*fromStdin {
		fmt.Fprint(out, "New admin password: ")
	}
	line, err := br.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return errors.New("no password provided")
	}
	password := strings.TrimRight(line, "\r\n")

	d, err := db.Open(configDir)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	name, err := auth.New(d).ResetAdmin(context.Background(), password)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Password for %q reset; all sessions revoked and login lockouts cleared.\n", name)
	return nil
}
