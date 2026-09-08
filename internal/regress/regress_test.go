// Package regress runs the SQL regression suite: every
// testdata/regress/sql/<name>.sql is run through a fresh Session and the
// output, formatted like psql -a -q, is compared with
// testdata/regress/expected/<name>.out. Run with -update to rewrite the
// expected files; actual output always goes to testdata/regress/results.
package regress

import (
	"bufio"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/session"
)

var update = flag.Bool("update", false, "rewrite the expected output files")

const root = "../../testdata/regress"

func TestRegress(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(root, "sql", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no regression files")
	}
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".sql")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			s, err := session.Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got := run(t, s, string(src))

			resultPath := filepath.Join(root, "results", name+".out")
			if err := os.WriteFile(resultPath, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			expectedPath := filepath.Join(root, "expected", name+".out")
			if *update {
				if err := os.WriteFile(expectedPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s:\n%s\nfull output in %s", expectedPath, diff(string(want), got), resultPath)
			}
		})
	}
}

// run feeds src to s the way psql -a -q would: every nonempty input line
// is echoed as it is read, and each statement runs as soon as its
// semicolon arrives, printing its result set or error. A
// reader error fails the test: it would otherwise look like the end of
// the file and compare a truncated run against a truncated expectation.
func run(t *testing.T, s *session.Session, src string) string {
	t.Helper()
	var out strings.Builder
	var buf string
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) != "" {
			out.WriteString(line + "\n")
		}
		buf += line + "\n"
		for {
			stmt, rest, ok := session.Split(buf)
			if !ok {
				break
			}
			buf = rest
			results, err := s.Exec(stmt)
			for _, r := range results {
				out.WriteString(r.String())
			}
			if err != nil {
				out.WriteString(report(err))
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the .sql file: %v", err)
	}
	return out.String()
}

func report(err error) string {
	var e *session.Error
	if errors.As(err, &e) {
		return e.Report()
	}
	return "ERROR:  " + err.Error() + "\n"
}

// diff shows the first differing line with some context.
func diff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			var b strings.Builder
			for j := max(0, i-3); j < i; j++ {
				b.WriteString("  " + g[j] + "\n")
			}
			b.WriteString("- " + wl + "\n+ " + gl + "\n")
			return strings.TrimSuffix("line "+strconv.Itoa(i+1)+":\n"+b.String(), "\n")
		}
	}
	return "(outputs differ only in length)"
}
