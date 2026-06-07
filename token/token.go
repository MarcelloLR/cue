// Package token defines the lexical tokens of the Cue language and the source
// positions that every token (and, downstream, every AST node) carries.
//
// Carrying precise spans from the very first stage is a foundational decision
// (see DESIGN.md §12): the structured-diagnostics contract depends on being
// able to point at the exact source range that produced an error.
package token

// TokenType identifies the lexical class of a token.
type TokenType string

// Position is a single point in the source text. Line and Col are 1-based for
// human-facing diagnostics; Offset is a 0-based byte index into the input.
type Position struct {
	Line   int
	Col    int
	Offset int
}

// Span is a half-open source range [Start, End) covering a token or AST node.
type Span struct {
	Start Position
	End   Position
}

// Token is a single lexical token plus the source range it came from.
type Token struct {
	Type    TokenType
	Literal string
	Span    Span
}

const (
	ILLEGAL TokenType = "ILLEGAL" // a byte/sequence we couldn't lex
	EOF     TokenType = "EOF"     // end of input

	// Identifiers and literals.
	IDENT  TokenType = "IDENT"
	INT    TokenType = "INT"
	FLOAT  TokenType = "FLOAT"
	STRING TokenType = "STRING"

	// Operators.
	ASSIGN   TokenType = "="
	PLUS     TokenType = "+"
	MINUS    TokenType = "-"
	BANG     TokenType = "!"
	ASTERISK TokenType = "*"
	SLASH    TokenType = "/"
	PERCENT  TokenType = "%"

	LT     TokenType = "<"
	GT     TokenType = ">"
	LE     TokenType = "<="
	GE     TokenType = ">="
	EQ     TokenType = "=="
	NOT_EQ TokenType = "!="
	AND    TokenType = "&&"
	OR     TokenType = "||"

	// Delimiters.
	DOT       TokenType = "."
	COMMA     TokenType = ","
	COLON     TokenType = ":"
	SEMICOLON TokenType = ";" // explicit ';' or an inserted statement terminator

	LPAREN   TokenType = "("
	RPAREN   TokenType = ")"
	LBRACE   TokenType = "{"
	RBRACE   TokenType = "}"
	LBRACKET TokenType = "["
	RBRACKET TokenType = "]"

	// Keywords.
	LET      TokenType = "LET"
	FN       TokenType = "FN"
	IF       TokenType = "IF"
	ELSE     TokenType = "ELSE"
	FOR      TokenType = "FOR"
	IN       TokenType = "IN"
	RETURN   TokenType = "RETURN"
	TRUE     TokenType = "TRUE"
	FALSE    TokenType = "FALSE"
	NULL     TokenType = "NULL"
	PARALLEL TokenType = "PARALLEL"
	RETRY    TokenType = "RETRY"
)

var keywords = map[string]TokenType{
	"let":      LET,
	"fn":       FN,
	"if":       IF,
	"else":     ELSE,
	"for":      FOR,
	"in":       IN,
	"return":   RETURN,
	"true":     TRUE,
	"false":    FALSE,
	"null":     NULL,
	"parallel": PARALLEL,
	"retry":    RETRY,
}

// LookupIdent maps an identifier to its keyword type, or IDENT if it is not a
// reserved word.
func LookupIdent(ident string) TokenType {
	if t, ok := keywords[ident]; ok {
		return t
	}
	return IDENT
}
