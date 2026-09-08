package lexer

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// kt is a token without a position, for table-driven tests.
type kt struct {
	Kind Kind
	Text string
}

func kinds(t *testing.T, src string) []kt {
	t.Helper()
	toks, err := Tokenize(src)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", src, err)
	}
	out := make([]kt, 0, len(toks))
	for _, tok := range toks {
		out = append(out, kt{tok.Kind, tok.Text})
	}
	return out
}

func TestTokens(t *testing.T) {
	eof := kt{EOF, ""}
	cases := []struct {
		src  string
		want []kt
	}{
		{"", []kt{eof}},
		{"   \t\n\r\f\v  ", []kt{eof}},
		{"select", []kt{{Keyword, "select"}, eof}},
		{"SELECT", []kt{{Keyword, "select"}, eof}},
		{"SeLeCt", []kt{{Keyword, "select"}, eof}},
		{"users", []kt{{Ident, "users"}, eof}},
		{"Users", []kt{{Ident, "users"}, eof}},
		{"_x1$", []kt{{Ident, "_x1$"}, eof}},
		{`"Users"`, []kt{{Ident, "Users"}, eof}},
		{`"select"`, []kt{{Ident, "select"}, eof}},
		{`"a""b"`, []kt{{Ident, `a"b`}, eof}},
		{`"with space"`, []kt{{Ident, "with space"}, eof}},
		{"42", []kt{{Integer, "42"}, eof}},
		{"0", []kt{{Integer, "0"}, eof}},
		{"007", []kt{{Integer, "007"}, eof}},
		{"3000000000000000000000", []kt{{Integer, "3000000000000000000000"}, eof}},
		{"-1", []kt{{Minus, "-"}, {Integer, "1"}, eof}},
		{"1.5", []kt{{Integer, "1"}, {Dot, "."}, {Integer, "5"}, eof}},
		{"''", []kt{{String, ""}, eof}},
		{"'abc'", []kt{{String, "abc"}, eof}},
		{"'it''s'", []kt{{String, "it's"}, eof}},
		{"''''", []kt{{String, "'"}, eof}},
		{"'a'''", []kt{{String, "a'"}, eof}},
		{"'a' 'b'", []kt{{String, "a"}, {String, "b"}, eof}},
		{"'a\nb'", []kt{{String, "a\nb"}, eof}},
		{`'a\b'`, []kt{{String, `a\b`}, eof}},
		{"'-- not a comment'", []kt{{String, "-- not a comment"}, eof}},
		{"= <> != < <= > >=", []kt{
			{Eq, "="}, {Ne, "<>"}, {Ne, "<>"}, {Lt, "<"}, {Le, "<="}, {Gt, ">"}, {Ge, ">="}, eof}},
		{"+ - * /", []kt{{Plus, "+"}, {Minus, "-"}, {Star, "*"}, {Slash, "/"}, eof}},
		{"(),;.", []kt{{LParen, "("}, {RParen, ")"}, {Comma, ","}, {Semicolon, ";"}, {Dot, "."}, eof}},
		{"<=>", []kt{{Le, "<="}, {Gt, ">"}, eof}},
		{"< >", []kt{{Lt, "<"}, {Gt, ">"}, eof}},
		{"a.b", []kt{{Ident, "a"}, {Dot, "."}, {Ident, "b"}, eof}},
		{"a-b", []kt{{Ident, "a"}, {Minus, "-"}, {Ident, "b"}, eof}},
		{"a--b", []kt{{Ident, "a"}, eof}},
		{"1+2", []kt{{Integer, "1"}, {Plus, "+"}, {Integer, "2"}, eof}},
		{"x=1", []kt{{Ident, "x"}, {Eq, "="}, {Integer, "1"}, eof}},
		{"select*from t", []kt{{Keyword, "select"}, {Star, "*"}, {Keyword, "from"}, {Ident, "t"}, eof}},
		{"-- comment", []kt{eof}},
		{"-- comment\nselect", []kt{{Keyword, "select"}, eof}},
		{"select -- c\n 1", []kt{{Keyword, "select"}, {Integer, "1"}, eof}},
		{"/* c */", []kt{eof}},
		{"/**/1", []kt{{Integer, "1"}, eof}},
		{"a/* c */b", []kt{{Ident, "a"}, {Ident, "b"}, eof}},
		{"/* a /* b */ c */ 1", []kt{{Integer, "1"}, eof}},
		{"/* -- */ 1", []kt{{Integer, "1"}, eof}},
		{"/* multi\nline */ 1", []kt{{Integer, "1"}, eof}},
		{"a/b", []kt{{Ident, "a"}, {Slash, "/"}, {Ident, "b"}, eof}},
		{"a*b", []kt{{Ident, "a"}, {Star, "*"}, {Ident, "b"}, eof}},
		{
			"SELECT id, name FROM users WHERE id >= 10 AND name <> 'x' ORDER BY id DESC LIMIT 5;",
			[]kt{
				{Keyword, "select"}, {Ident, "id"}, {Comma, ","}, {Ident, "name"},
				{Keyword, "from"}, {Ident, "users"},
				{Keyword, "where"}, {Ident, "id"}, {Ge, ">="}, {Integer, "10"},
				{Keyword, "and"}, {Ident, "name"}, {Ne, "<>"}, {String, "x"},
				{Keyword, "order"}, {Keyword, "by"}, {Ident, "id"}, {Keyword, "desc"},
				{Keyword, "limit"}, {Integer, "5"}, {Semicolon, ";"}, eof,
			},
		},
		{
			"INSERT INTO t (a, b) VALUES (1, 'x'), (-2, NULL)",
			[]kt{
				{Keyword, "insert"}, {Keyword, "into"}, {Ident, "t"},
				{LParen, "("}, {Ident, "a"}, {Comma, ","}, {Ident, "b"}, {RParen, ")"},
				{Keyword, "values"},
				{LParen, "("}, {Integer, "1"}, {Comma, ","}, {String, "x"}, {RParen, ")"}, {Comma, ","},
				{LParen, "("}, {Minus, "-"}, {Integer, "2"}, {Comma, ","}, {Keyword, "null"}, {RParen, ")"},
				eof,
			},
		},
	}
	for _, c := range cases {
		got := kinds(t, c.src)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokenize(%q)\n got %v\nwant %v", c.src, got, c.want)
		}
	}
}

func TestAllKeywords(t *testing.T) {
	for kw := range keywords {
		got := kinds(t, strings.ToUpper(kw))
		want := []kt{{Keyword, kw}, {EOF, ""}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v", kw, got)
		}
		got = kinds(t, `"`+kw+`"`)
		want = []kt{{Ident, kw}, {EOF, ""}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("quoted %s: got %v", kw, got)
		}
	}
}

func TestIdentifierFolding(t *testing.T) {
	cases := []struct{ src, want string }{
		{"abc", "abc"},
		{"ABC", "abc"},
		{"aBc_D9", "abc_d9"},
		{"Straße", "straße"},
		{"Über", "Über"},
		{"日本", "日本"},
		{`"ABC"`, "ABC"},
		{`"Straße"`, "Straße"},
		{`""""`, `"`},
	}
	for _, c := range cases {
		got := kinds(t, c.src)
		want := []kt{{Ident, c.want}, {EOF, ""}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %v, want %v", c.src, got, want)
		}
	}
}

func TestPositions(t *testing.T) {
	src := "select a,\n\tb -- c\r\n  from \"T\" /* x\ny */ 'q'\nwhere ä = 1"
	toks, err := Tokenize(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Keyword, "select", Pos{0, 1, 1}},
		{Ident, "a", Pos{7, 1, 8}},
		{Comma, ",", Pos{8, 1, 9}},
		{Ident, "b", Pos{11, 2, 2}},
		{Keyword, "from", Pos{21, 3, 3}},
		{Ident, "T", Pos{26, 3, 8}},
		{String, "q", Pos{40, 4, 6}},
		{Keyword, "where", Pos{44, 5, 1}},
		{Ident, "ä", Pos{50, 5, 7}},
		{Eq, "=", Pos{53, 5, 9}},
		{Integer, "1", Pos{55, 5, 11}},
		{EOF, "", Pos{56, 5, 12}},
	}
	if !reflect.DeepEqual(toks, want) {
		for i := range max(len(toks), len(want)) {
			var g, w Token
			if i < len(toks) {
				g = toks[i]
			}
			if i < len(want) {
				w = want[i]
			}
			if g != w {
				t.Errorf("token %d: got %+v, want %+v", i, g, w)
			}
		}
	}
}

func TestEOFPosition(t *testing.T) {
	cases := []struct {
		src  string
		want Pos
	}{
		{"", Pos{0, 1, 1}},
		{"a", Pos{1, 1, 2}},
		{"a\n", Pos{2, 2, 1}},
		{"a\r\n", Pos{3, 2, 1}},
		{"ä", Pos{2, 1, 2}},
		{"1 -- c", Pos{6, 1, 7}},
	}
	for _, c := range cases {
		toks, err := Tokenize(c.src)
		if err != nil {
			t.Fatal(err)
		}
		if got := toks[len(toks)-1]; got.Kind != EOF || got.Pos != c.want {
			t.Errorf("Tokenize(%q) last = %+v, want EOF at %+v", c.src, got, c.want)
		}
	}
}

func TestNextAfterEOF(t *testing.T) {
	l := New("1")
	if tok, err := l.Next(); err != nil || tok.Kind != Integer {
		t.Fatalf("first: %+v, %v", tok, err)
	}
	for i := range 3 {
		tok, err := l.Next()
		if err != nil || tok.Kind != EOF || tok.Pos != (Pos{1, 1, 2}) {
			t.Fatalf("call %d: %+v, %v", i, tok, err)
		}
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		src  string
		want error
		pos  Pos
	}{
		{"'abc", ErrUnterminatedString, Pos{0, 1, 1}},
		{"select 'a''b", ErrUnterminatedString, Pos{7, 1, 8}},
		{"'a''", ErrUnterminatedString, Pos{0, 1, 1}},
		{"x\n  'abc", ErrUnterminatedString, Pos{4, 2, 3}},
		{`"abc`, ErrUnterminatedIdent, Pos{0, 1, 1}},
		{`"a""`, ErrUnterminatedIdent, Pos{0, 1, 1}},
		{`""`, ErrEmptyIdent, Pos{0, 1, 1}},
		{`select ""`, ErrEmptyIdent, Pos{7, 1, 8}},
		{"/* abc", ErrUnterminatedComment, Pos{0, 1, 1}},
		{"1 /* a /* b */", ErrUnterminatedComment, Pos{2, 1, 3}},
		{"/* a *", ErrUnterminatedComment, Pos{0, 1, 1}},
		{"123abc", ErrTrailingJunk, Pos{0, 1, 1}},
		{"1_000", ErrTrailingJunk, Pos{0, 1, 1}},
		{"x = 9z", ErrTrailingJunk, Pos{4, 1, 5}},
		{"@", ErrBadChar, Pos{0, 1, 1}},
		{"a # b", ErrBadChar, Pos{2, 1, 3}},
		{"$1", ErrBadChar, Pos{0, 1, 1}},
		{"!", ErrBadChar, Pos{0, 1, 1}},
		{"!x", ErrBadChar, Pos{0, 1, 1}},
		{"a::int", ErrBadChar, Pos{1, 1, 2}},
		{"a\n\nb ` c", ErrBadChar, Pos{5, 3, 3}},
		{"\x00", ErrBadChar, Pos{0, 1, 1}},
	}
	for _, c := range cases {
		toks, err := Tokenize(c.src)
		if toks != nil {
			t.Errorf("Tokenize(%q) returned tokens with error", c.src)
		}
		if !errors.Is(err, c.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", c.src, err, c.want)
			continue
		}
		var le *Error
		if !errors.As(err, &le) {
			t.Errorf("Tokenize(%q): error is %T, want *Error", c.src, err)
			continue
		}
		if le.Pos != c.pos {
			t.Errorf("Tokenize(%q) at %+v, want %+v", c.src, le.Pos, c.pos)
		}
		if !strings.Contains(le.Error(), c.want.Error()) {
			t.Errorf("Tokenize(%q): message %q lacks %q", c.src, le.Error(), c.want.Error())
		}
	}
}

func TestErrorIsSticky(t *testing.T) {
	l := New("1 @ 2")
	if _, err := l.Next(); err != nil {
		t.Fatal(err)
	}
	_, err1 := l.Next()
	if !errors.Is(err1, ErrBadChar) {
		t.Fatalf("got %v", err1)
	}
	_, err2 := l.Next()
	if err2 != err1 {
		t.Fatalf("second call: %v, want the same error", err2)
	}
}

func TestKindString(t *testing.T) {
	cases := map[Kind]string{
		EOF: "end of input", Ident: "identifier", Keyword: "keyword",
		Integer: "integer", String: "string", Eq: "=", Ne: "<>", Lt: "<",
		Le: "<=", Gt: ">", Ge: ">=", Plus: "+", Minus: "-", Star: "*",
		Slash: "/", LParen: "(", RParen: ")", Comma: ",", Semicolon: ";",
		Dot: ".",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(k), got, want)
		}
	}
}

func FuzzTokenize(f *testing.F) {
	for _, s := range []string{
		"", "select 1", "'a''b'", `"x"`, "/* a /* b */ c */", "-- c\n1",
		"123abc", "'unterminated", "a <= b <> c != d", "Straße", "\x00", "\xff",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, err := Tokenize(src)
		if err != nil {
			var le *Error
			if !errors.As(err, &le) {
				t.Fatalf("error is %T", err)
			}
			if le.Pos.Offset < 0 || le.Pos.Offset > len(src) || le.Pos.Line < 1 || le.Pos.Column < 1 {
				t.Fatalf("bad error position %+v", le.Pos)
			}
			return
		}
		if len(toks) == 0 || toks[len(toks)-1].Kind != EOF {
			t.Fatal("token stream does not end with EOF")
		}
		prev := -1
		for _, tok := range toks {
			if tok.Pos.Offset < 0 || tok.Pos.Offset > len(src) {
				t.Fatalf("token %+v out of range", tok)
			}
			if tok.Pos.Offset <= prev {
				t.Fatalf("offsets do not increase at %+v", tok)
			}
			if tok.Kind == Ident && !utf8.ValidString(tok.Text) && utf8.ValidString(src) {
				t.Fatalf("identifier %q is not valid UTF-8", tok.Text)
			}
			prev = tok.Pos.Offset
		}
	})
}
