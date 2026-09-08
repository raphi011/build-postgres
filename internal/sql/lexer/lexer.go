// Package lexer turns SQL text into tokens. See chapters/06-lexer.
package lexer

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind classifies a token.
type Kind int

const (
	EOF Kind = iota
	Ident
	Keyword
	Integer
	String
	Eq        // =
	Ne        // <> (also written !=)
	Lt        // <
	Le        // <=
	Gt        // >
	Ge        // >=
	Plus      // +
	Minus     // -
	Star      // *
	Slash     // /
	LParen    // (
	RParen    // )
	Comma     // ,
	Semicolon // ;
	Dot       // .
)

var kindNames = [...]string{
	EOF:       "end of input",
	Ident:     "identifier",
	Keyword:   "keyword",
	Integer:   "integer",
	String:    "string",
	Eq:        "=",
	Ne:        "<>",
	Lt:        "<",
	Le:        "<=",
	Gt:        ">",
	Ge:        ">=",
	Plus:      "+",
	Minus:     "-",
	Star:      "*",
	Slash:     "/",
	LParen:    "(",
	RParen:    ")",
	Comma:     ",",
	Semicolon: ";",
	Dot:       ".",
}

// String returns the operator text, or a description for the other kinds.
func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("Kind(%d)", int(k))
	}
	return kindNames[k]
}

// Pos is a position in the source text. Offset is a 0-based byte offset;
// Line and Column are 1-based, with Column counted in characters.
type Pos struct {
	Offset int
	Line   int
	Column int
}

// Token is one lexical unit of the input.
type Token struct {
	Kind Kind
	Text string
	Pos  Pos
}

// keywords is the set of reserved words, lower-cased. Later chapters add
// to it.
var keywords = map[string]bool{
	"and": true, "as": true, "asc": true, "begin": true, "by": true,
	"commit": true, "create": true, "delete": true, "desc": true,
	"drop": true, "explain": true, "false": true, "from": true,
	"insert": true, "into": true, "is": true, "join": true, "key": true,
	"limit": true, "not": true, "null": true, "on": true, "or": true,
	"order": true, "primary": true, "rollback": true, "select": true,
	"set": true, "table": true, "true": true, "update": true,
	"values": true, "where": true,
}

var (
	ErrUnterminatedString  = errors.New("unterminated quoted string")
	ErrUnterminatedIdent   = errors.New("unterminated quoted identifier")
	ErrUnterminatedComment = errors.New("unterminated /* comment")
	ErrEmptyIdent          = errors.New("zero-length delimited identifier")
	ErrTrailingJunk        = errors.New("trailing junk after numeric literal")
	ErrBadChar             = errors.New("unexpected character")
)

// Error is a lexical error at a position. It unwraps to one of the
// sentinel errors above.
type Error struct {
	Pos Pos
	Err error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%v at line %d, column %d", e.Err, e.Pos.Line, e.Pos.Column)
}

func (e *Error) Unwrap() error { return e.Err }

// Lexer reads tokens from a source string.
type Lexer struct {
	src  string
	off  int
	line int
	col  int
	err  error
}

// New returns a lexer positioned at the start of src.
// PostgreSQL: scanner_init in scan.l.
func New(src string) *Lexer {
	return &Lexer{src: src, line: 1, col: 1}
}

// pos returns the current position.
func (l *Lexer) pos() Pos { return Pos{l.off, l.line, l.col} }

// peek returns the byte at offset off+n, or 0 past the end.
func (l *Lexer) peek(n int) byte {
	if l.off+n < len(l.src) {
		return l.src[l.off+n]
	}
	return 0
}

// advance consumes n bytes, tracking line and column. Callers only pass
// n that ends on a character boundary.
func (l *Lexer) advance(n int) {
	for i := 0; i < n; {
		c := l.src[l.off]
		switch {
		case c == '\n':
			l.line++
			l.col = 1
			l.off++
			i++
		case c < utf8.RuneSelf:
			l.col++
			l.off++
			i++
		default:
			_, size := utf8.DecodeRuneInString(l.src[l.off:])
			l.col++
			l.off += size
			i += size
		}
	}
}

func (l *Lexer) fail(at Pos, err error) (Token, error) {
	l.err = &Error{Pos: at, Err: err}
	return Token{}, l.err
}

// Next returns the next token, or EOF at the end of input. After an error
// every call returns the same error.
// Returns a *Error wrapping ErrUnterminatedString or ErrUnterminatedIdent
// if a quote is still open at end of input; ErrUnterminatedComment if a
// /* is; ErrEmptyIdent for ""; ErrTrailingJunk if an identifier character
// follows a digit run; ErrBadChar for any other character.
// PostgreSQL: core_yylex in scan.l.
func (l *Lexer) Next() (Token, error) {
	if l.err != nil {
		return Token{}, l.err
	}
	if err := l.skipSpace(); err != nil {
		return Token{}, err
	}
	start := l.pos()
	if l.off >= len(l.src) {
		return Token{Kind: EOF, Pos: start}, nil
	}
	c := l.peek(0)
	switch {
	case isDigit(c):
		return l.integer(start)
	case c == '\'':
		return l.str(start)
	case c == '"':
		return l.quotedIdent(start)
	case isIdentStart(c):
		return l.ident(start)
	}
	if k, n := operator(c, l.peek(1)); k != EOF {
		l.advance(n)
		return Token{Kind: k, Text: k.String(), Pos: start}, nil
	}
	return l.fail(start, ErrBadChar)
}

// operator matches the longest operator at c, c2. It returns EOF and 0 if
// there is none.
func operator(c, c2 byte) (Kind, int) {
	switch c {
	case '<':
		switch c2 {
		case '=':
			return Le, 2
		case '>':
			return Ne, 2
		}
		return Lt, 1
	case '>':
		if c2 == '=' {
			return Ge, 2
		}
		return Gt, 1
	case '!':
		if c2 == '=' {
			return Ne, 2
		}
	case '=':
		return Eq, 1
	case '+':
		return Plus, 1
	case '-':
		return Minus, 1
	case '*':
		return Star, 1
	case '/':
		return Slash, 1
	case '(':
		return LParen, 1
	case ')':
		return RParen, 1
	case ',':
		return Comma, 1
	case ';':
		return Semicolon, 1
	case '.':
		return Dot, 1
	}
	return EOF, 0
}

// skipSpace consumes whitespace and comments.
func (l *Lexer) skipSpace() error {
	for l.off < len(l.src) {
		c := l.peek(0)
		switch {
		case isSpace(c):
			l.advance(1)
		case c == '-' && l.peek(1) == '-':
			for l.off < len(l.src) && l.peek(0) != '\n' {
				l.advance(1)
			}
		case c == '/' && l.peek(1) == '*':
			start := l.pos()
			l.advance(2)
			depth := 1
			for depth > 0 {
				if l.off >= len(l.src) {
					_, err := l.fail(start, ErrUnterminatedComment)
					return err
				}
				switch {
				case l.peek(0) == '/' && l.peek(1) == '*':
					depth++
					l.advance(2)
				case l.peek(0) == '*' && l.peek(1) == '/':
					depth--
					l.advance(2)
				default:
					l.advance(1)
				}
			}
		default:
			return nil
		}
	}
	return nil
}

func (l *Lexer) integer(start Pos) (Token, error) {
	for isDigit(l.peek(0)) {
		l.advance(1)
	}
	if isIdentStart(l.peek(0)) {
		return l.fail(start, ErrTrailingJunk)
	}
	return Token{Kind: Integer, Text: l.src[start.Offset:l.off], Pos: start}, nil
}

// quoted consumes a literal delimited by q, in which qq stands for q, and
// returns its contents. The opening quote has not been consumed yet.
func (l *Lexer) quoted(q byte) (string, bool) {
	l.advance(1)
	var sb strings.Builder
	for l.off < len(l.src) {
		c := l.peek(0)
		if c != q {
			// Copy whole characters so column counting stays right.
			n := 1
			if c >= utf8.RuneSelf {
				_, n = utf8.DecodeRuneInString(l.src[l.off:])
			}
			sb.WriteString(l.src[l.off : l.off+n])
			l.advance(n)
			continue
		}
		if l.peek(1) == q {
			sb.WriteByte(q)
			l.advance(2)
			continue
		}
		l.advance(1)
		return sb.String(), true
	}
	return "", false
}

func (l *Lexer) str(start Pos) (Token, error) {
	s, ok := l.quoted('\'')
	if !ok {
		return l.fail(start, ErrUnterminatedString)
	}
	return Token{Kind: String, Text: s, Pos: start}, nil
}

func (l *Lexer) quotedIdent(start Pos) (Token, error) {
	s, ok := l.quoted('"')
	if !ok {
		return l.fail(start, ErrUnterminatedIdent)
	}
	if s == "" {
		return l.fail(start, ErrEmptyIdent)
	}
	return Token{Kind: Ident, Text: s, Pos: start}, nil
}

func (l *Lexer) ident(start Pos) (Token, error) {
	for l.off < len(l.src) && isIdentChar(l.peek(0)) {
		if l.peek(0) < utf8.RuneSelf {
			l.advance(1)
		} else {
			_, n := utf8.DecodeRuneInString(l.src[l.off:])
			l.advance(n)
		}
	}
	text := downcase(l.src[start.Offset:l.off])
	if keywords[text] {
		return Token{Kind: Keyword, Text: text, Pos: start}, nil
	}
	return Token{Kind: Ident, Text: text, Pos: start}, nil
}

// downcase folds ASCII letters only, like PostgreSQL's downcase_identifier.
func downcase(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c >= utf8.RuneSelf
}

func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// Tokenize returns every token in src, ending with EOF.
func Tokenize(src string) ([]Token, error) {
	l := New(src)
	var toks []Token
	for {
		tok, err := l.Next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
		if tok.Kind == EOF {
			return toks, nil
		}
	}
}
