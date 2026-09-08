// Command pgdb is an interactive SQL shell over a data directory, in the
// style of psql. See chapters/11-executor.
//
// Usage: pgdb DATADIR
//
//	pgdb sample DIR       write a small relation to a fresh directory
//	pgdb dump FILE [BLOCK]  print the pages of a relation file
//
// The data directory is bootstrapped if it does not exist. Statements end
// with a semicolon and may span lines. Backslash commands: \d [table],
// \dt, \q, \?.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/raphi011/build-postgres/internal/session"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "sample":
		if len(args) != 2 {
			usage()
		}
		run(sample(args[1], os.Stdout))
		return
	case len(args) > 0 && args[0] == "dump":
		if len(args) != 2 && len(args) != 3 {
			usage()
		}
		blk := -1
		if len(args) == 3 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "pgdb: BLOCK must be a block number")
				os.Exit(2)
			}
			blk = n
		}
		run(dump(args[1], blk, os.Stdout))
		return
	case len(args) != 1 || strings.HasPrefix(args[0], "-"):
		usage()
	}
	s, err := session.Open(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgdb:", err)
		os.Exit(1)
	}
	info, _ := os.Stdin.Stat()
	interactive := info != nil && info.Mode()&os.ModeCharDevice != 0
	rerr := repl(s, os.Stdin, os.Stdout, os.Stderr, interactive)
	cerr := s.Close()
	if rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "pgdb:", rerr)
		os.Exit(1)
	}
}

// usage prints how to invoke pgdb and exits.
func usage() {
	fmt.Fprintln(os.Stderr, "usage: pgdb DATADIR")
	fmt.Fprintln(os.Stderr, "       pgdb sample DIR")
	fmt.Fprintln(os.Stderr, "       pgdb dump FILE [BLOCK]")
	os.Exit(2)
}

// run exits with a message when a subcommand fails.
func run(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgdb:", err)
		os.Exit(1)
	}
}

// repl reads statements from in until EOF or \q. It returns the reader's
// error, if any: a line longer than the scanner's buffer looks exactly
// like end of input, and silently running a third of a piped script is
// worse than refusing it.
func repl(s *session.Session, in io.Reader, out, errOut io.Writer, interactive bool) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var buf string
	for {
		if interactive {
			if buf == "" {
				fmt.Fprint(out, "pgdb=# ")
			} else {
				fmt.Fprint(out, "pgdb-# ")
			}
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return fmt.Errorf("reading input: %w", err)
			}
			return nil
		}
		line := sc.Text()
		if buf == "" && strings.HasPrefix(strings.TrimSpace(line), `\`) {
			if !command(s, strings.TrimSpace(line), out, errOut) {
				return nil
			}
			continue
		}
		buf += line + "\n"
		for {
			stmt, rest, ok := session.Split(buf)
			if !ok {
				break
			}
			buf = rest
			exec(s, stmt, out, errOut)
		}
		if strings.TrimSpace(buf) == "" {
			buf = ""
		}
	}
}

func exec(s *session.Session, stmt string, out, errOut io.Writer) {
	results, err := s.Exec(stmt)
	for _, r := range results {
		if r.Columns != nil {
			fmt.Fprint(out, r)
		} else {
			fmt.Fprintln(out, r.Tag)
		}
	}
	if err != nil {
		report(err, errOut)
	}
}

func report(err error, errOut io.Writer) {
	var e *session.Error
	if errors.As(err, &e) {
		fmt.Fprint(errOut, e.Report())
		return
	}
	fmt.Fprintln(errOut, "ERROR: ", err)
}

// command runs a backslash command and reports whether to keep going.
func command(s *session.Session, line string, out, errOut io.Writer) bool {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	var r *session.Result
	var err error
	switch name {
	case `\q`:
		return false
	case `\?`:
		fmt.Fprint(out, "  \\d NAME   describe table\n  \\d, \\dt   list tables\n  \\q        quit\n")
		return true
	case `\dt`:
		r, err = s.Tables()
	case `\d`:
		if arg == "" {
			r, err = s.Tables()
		} else {
			r, err = s.Describe(arg)
		}
	default:
		fmt.Fprintf(errOut, "invalid command %s\nTry \\? for help.\n", name)
		return true
	}
	if err != nil {
		report(err, errOut)
		return true
	}
	fmt.Fprint(out, r)
	return true
}
