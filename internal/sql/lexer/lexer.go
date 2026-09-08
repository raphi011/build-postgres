// Package lexer turns SQL text into tokens. See chapters/06-lexer.
package lexer

import (
	"errors"
	"fmt"
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
	"index": true, "insert": true, "into": true, "is": true, "join": true,
	"key": true, "limit": true, "not": true, "null": true, "on": true,
	"or": true, "order": true, "primary": true, "rollback": true,
	"select": true, "set": true, "table": true, "true": true,
	"unique": true, "update": true, "values": true, "where": true,
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
	panic("not implemented")
}

// Next returns the next token, or EOF at the end of input. After an error
// every call returns the same error.
// Returns a *Error wrapping ErrUnterminatedString or ErrUnterminatedIdent
// if a quote is still open at end of input; ErrUnterminatedComment if a
// /* is; ErrEmptyIdent for ""; ErrTrailingJunk if an identifier character
// follows a digit run; ErrBadChar for any other character.
// PostgreSQL: core_yylex in scan.l.
func (l *Lexer) Next() (Token, error) {
	panic("not implemented")
}

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

// IsKeyword reports whether word (already lower-cased) is a keyword.
func IsKeyword(word string) bool { return keywords[word] }
