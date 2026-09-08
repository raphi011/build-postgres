// Package isolation runs the isolation test suite: every
// testdata/isolation/specs/<name>.spec declares setup and teardown SQL,
// sessions with named steps, and permutations of those steps; the
// runner plays each permutation against fresh sessions of one cluster,
// noting where a step blocks on another session, and compares the
// output with testdata/isolation/expected/<name>.out. Run with -update
// to rewrite the expected files; actual output always goes to
// testdata/isolation/results.
// PostgreSQL: src/test/isolation/isolationtester.c.
package isolation

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/raphi011/build-postgres/internal/session"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var update = flag.Bool("update", false, "rewrite the expected output files")

const root = "../../testdata/isolation"

func TestIsolation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(root, "specs", "*.spec"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no isolation specs")
	}
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".spec")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			sp, err := parse(string(src))
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			got := run(t, sp)

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

// A spec file, in the format of src/test/isolation/specs:
//
//	setup { SQL }            run once before each permutation
//	teardown { SQL }         run once after each permutation
//	session NAME             a session; its steps follow
//	  setup { SQL }          run on the session before each permutation
//	  teardown { SQL }       run on the session after each permutation
//	  step NAME { SQL }      a step
//	permutation NAME ...     the steps to run, in order, one per line
//
// A # starts a comment. Without a permutation line, every step runs
// once in the order declared.
type spec struct {
	setup, teardown string
	sessions        []*sessionSpec
	steps           map[string]*step
	permutations    [][]*step
}

type sessionSpec struct {
	name            string
	setup, teardown string
	steps           []*step
}

type step struct {
	name    string
	sql     string
	session *sessionSpec
}

// tokens splits a spec into words, {...} blocks (the braces stripped),
// and newlines.
func tokens(src string) ([]string, error) {
	var toks []string
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			toks = append(toks, "\n")
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{':
			j := strings.IndexByte(src[i:], '}')
			if j < 0 {
				return nil, errors.New("unterminated {")
			}
			toks = append(toks, "{"+strings.TrimSpace(src[i+1:i+j]))
			i += j + 1
		default:
			j := i
			for j < len(src) && !strings.ContainsRune(" \t\r\n{#", rune(src[j])) {
				j++
			}
			toks = append(toks, src[i:j])
			i = j
		}
	}
	return toks, nil
}

func parse(src string) (*spec, error) {
	toks, err := tokens(src)
	if err != nil {
		return nil, err
	}
	sp := &spec{steps: map[string]*step{}}
	var cur *sessionSpec
	// block returns the { SQL } at or after i, newlines skipped, and
	// its index.
	block := func(i int) (string, int, error) {
		for i < len(toks) && toks[i] == "\n" {
			i++
		}
		if i >= len(toks) || !strings.HasPrefix(toks[i], "{") {
			return "", i, fmt.Errorf("expected { SQL } after %s", toks[i-1])
		}
		return toks[i][1:], i, nil
	}
	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "\n":
		case "setup", "teardown":
			sql, j, err := block(i + 1)
			if err != nil {
				return nil, err
			}
			switch {
			case cur == nil && toks[i] == "setup":
				sp.setup = sql
			case cur == nil:
				sp.teardown = sql
			case toks[i] == "setup":
				cur.setup = sql
			default:
				cur.teardown = sql
			}
			i = j
		case "session":
			if i+1 >= len(toks) {
				return nil, errors.New("session without a name")
			}
			cur = &sessionSpec{name: toks[i+1]}
			sp.sessions = append(sp.sessions, cur)
			i++
		case "step":
			if cur == nil {
				return nil, errors.New("step outside a session")
			}
			if i+1 >= len(toks) {
				return nil, errors.New("step without a name")
			}
			sql, j, err := block(i + 2)
			if err != nil {
				return nil, err
			}
			st := &step{name: toks[i+1], sql: sql, session: cur}
			if _, dup := sp.steps[st.name]; dup {
				return nil, fmt.Errorf("step %s declared twice", st.name)
			}
			sp.steps[st.name] = st
			cur.steps = append(cur.steps, st)
			i = j
		case "permutation":
			var perm []*step
			for i+1 < len(toks) && toks[i+1] != "\n" {
				i++
				st, ok := sp.steps[toks[i]]
				if !ok {
					return nil, fmt.Errorf("permutation names unknown step %s", toks[i])
				}
				perm = append(perm, st)
			}
			sp.permutations = append(sp.permutations, perm)
		default:
			return nil, fmt.Errorf("unexpected %q", toks[i])
		}
	}
	if len(sp.sessions) == 0 {
		return nil, errors.New("no sessions")
	}
	if len(sp.permutations) == 0 {
		var all []*step
		for _, s := range sp.sessions {
			all = append(all, s.steps...)
		}
		sp.permutations = [][]*step{all}
	}
	return sp, nil
}

// running is a step in progress on its session's goroutine.
type running struct {
	step *step
	conn *session.Session
	done chan string // the step's output, once
}

// run plays every permutation and returns the output.
func run(t *testing.T, sp *spec) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Parsed test spec with %d sessions\n", len(sp.sessions))
	for _, perm := range sp.permutations {
		names := make([]string, len(perm))
		for i, st := range perm {
			names[i] = st.name
		}
		fmt.Fprintf(&out, "\nstarting permutation: %s\n", strings.Join(names, " "))
		permutation(t, sp, perm, &out)
	}
	return out.String()
}

// permutation runs one permutation on a fresh cluster. A step that
// blocks on another session is reported as waiting and completed when
// a later step unblocks it, as PostgreSQL's isolationtester does.
func permutation(t *testing.T, sp *spec, perm []*step, out *strings.Builder) {
	c, err := session.OpenCluster(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	control, err := c.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	mustExec(t, control, sp.setup)
	conns := map[*sessionSpec]*session.Session{}
	for _, s := range sp.sessions {
		conn, err := c.Connect()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conns[s] = conn
		mustExec(t, conn, s.setup)
	}

	var waiting []*running
	complete := func(r *running) {
		fmt.Fprintf(out, "step %s: <... completed>\n%s", r.step.name, <-r.done)
	}
	// finish prints every waiting step that can complete now, in order,
	// and keeps the ones still blocked.
	finish := func() {
		var still []*running
		for _, r := range waiting {
			for {
				select {
				case res := <-r.done:
					r.done <- res
					complete(r)
				default:
					if r.conn.Blocked() {
						still = append(still, r)
					} else {
						time.Sleep(time.Millisecond)
						continue
					}
				}
				break
			}
		}
		waiting = still
	}
	for _, st := range perm {
		// A session runs one step at a time: its blocked step must end
		// before the next begins.
		var still []*running
		for _, r := range waiting {
			if r.step.session == st.session {
				complete(r)
			} else {
				still = append(still, r)
			}
		}
		waiting = still
		r := start(st, conns[st.session])
		for {
			select {
			case res := <-r.done:
				fmt.Fprintf(out, "step %s: %s\n%s", st.name, st.sql, res)
			default:
				if !r.conn.Blocked() {
					time.Sleep(time.Millisecond)
					continue
				}
				fmt.Fprintf(out, "step %s: %s <waiting ...>\n", st.name, st.sql)
				waiting = append(waiting, r)
			}
			break
		}
		finish()
	}
	for _, r := range waiting {
		complete(r)
	}
	for _, s := range sp.sessions {
		conns[s].Close()
		if s.teardown != "" {
			mustExec(t, control, s.teardown)
		}
	}
	mustExec(t, control, sp.teardown)
}

// start runs a step on its session's goroutine.
func start(st *step, conn *session.Session) *running {
	r := &running{step: st, conn: conn, done: make(chan string, 1)}
	go func() {
		results, err := conn.Exec(st.sql)
		var b strings.Builder
		for _, res := range results {
			for _, w := range res.Warnings {
				b.WriteString("WARNING:  " + w + "\n")
			}
			b.WriteString(format(res))
		}
		if err != nil {
			var e *session.Error
			if errors.As(err, &e) {
				b.WriteString("ERROR:  " + e.Msg + "\n")
			} else {
				b.WriteString("ERROR:  " + err.Error() + "\n")
			}
		}
		r.done <- b.String()
	}()
	return r
}

func mustExec(t *testing.T, s *session.Session, sql string) {
	t.Helper()
	if strings.TrimSpace(sql) == "" {
		return
	}
	if _, err := s.Exec(sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// format renders a result set the way isolationtester prints one: the
// column names, a separator, and the rows, columns joined by | and
// padded to the widest value, numbers right-aligned; then the row
// count and a blank line. Statements without rows print nothing.
// PostgreSQL: PQprint in fe-print.c, as printResultSet calls it.
func format(r *session.Result) string {
	if r.Columns == nil {
		return ""
	}
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = utf8.RuneCountInString(c.Name)
	}
	cells := make([][]string, len(r.Rows))
	for i, row := range r.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			cells[i][j] = session.FormatDatum(v)
			widths[j] = max(widths[j], utf8.RuneCountInString(cells[i][j]))
		}
	}
	var b strings.Builder
	for i, c := range r.Columns {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(pad(c.Name, widths[i], false))
	}
	b.WriteByte('\n')
	for i, w := range widths {
		if i > 0 {
			b.WriteByte('+')
		}
		b.WriteString(strings.Repeat("-", w))
	}
	b.WriteByte('\n')
	for _, row := range cells {
		for j, s := range row {
			if j > 0 {
				b.WriteByte('|')
			}
			typ := r.Columns[j].Type
			b.WriteString(pad(s, widths[j], typ == tuple.Int4 || typ == tuple.Int8))
		}
		b.WriteByte('\n')
	}
	if len(r.Rows) == 1 {
		b.WriteString("(1 row)\n\n")
	} else {
		b.WriteString("(" + strconv.Itoa(len(r.Rows)) + " rows)\n\n")
	}
	return b.String()
}

func pad(s string, width int, right bool) string {
	fill := strings.Repeat(" ", width-utf8.RuneCountInString(s))
	if right {
		return fill + s
	}
	return s + fill
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
