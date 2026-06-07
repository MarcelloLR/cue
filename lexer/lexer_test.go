package lexer

import (
	"testing"

	"github.com/MarcelloLR/cue/token"
)

func TestNextTokenBasic(t *testing.T) {
	input := `let five = 5
let pi = 3.14
let add = fn(x, y) { x + y }
result = add(five, pi)
[1, 2][0]
{"k": v}.k
10 >= 2 && !false || 1 != 2
// a comment
"hi\n"`

	tests := []struct {
		wantType    token.TokenType
		wantLiteral string
	}{
		{token.LET, "let"},
		{token.IDENT, "five"},
		{token.ASSIGN, "="},
		{token.INT, "5"},
		{token.SEMICOLON, "\n"},

		{token.LET, "let"},
		{token.IDENT, "pi"},
		{token.ASSIGN, "="},
		{token.FLOAT, "3.14"},
		{token.SEMICOLON, "\n"},

		{token.LET, "let"},
		{token.IDENT, "add"},
		{token.ASSIGN, "="},
		{token.FN, "fn"},
		{token.LPAREN, "("},
		{token.IDENT, "x"},
		{token.COMMA, ","},
		{token.IDENT, "y"},
		{token.RPAREN, ")"},
		{token.LBRACE, "{"},
		{token.IDENT, "x"},
		{token.PLUS, "+"},
		{token.IDENT, "y"},
		{token.RBRACE, "}"},
		{token.SEMICOLON, "\n"},

		{token.IDENT, "result"},
		{token.ASSIGN, "="},
		{token.IDENT, "add"},
		{token.LPAREN, "("},
		{token.IDENT, "five"},
		{token.COMMA, ","},
		{token.IDENT, "pi"},
		{token.RPAREN, ")"},
		{token.SEMICOLON, "\n"},

		{token.LBRACKET, "["},
		{token.INT, "1"},
		{token.COMMA, ","},
		{token.INT, "2"},
		{token.RBRACKET, "]"},
		{token.LBRACKET, "["},
		{token.INT, "0"},
		{token.RBRACKET, "]"},
		{token.SEMICOLON, "\n"},

		{token.LBRACE, "{"},
		{token.STRING, "k"},
		{token.COLON, ":"},
		{token.IDENT, "v"},
		{token.RBRACE, "}"},
		{token.DOT, "."},
		{token.IDENT, "k"},
		{token.SEMICOLON, "\n"},

		{token.INT, "10"},
		{token.GE, ">="},
		{token.INT, "2"},
		{token.AND, "&&"},
		{token.BANG, "!"},
		{token.FALSE, "false"},
		{token.OR, "||"},
		{token.INT, "1"},
		{token.NOT_EQ, "!="},
		{token.INT, "2"},
		{token.SEMICOLON, "\n"},

		// comment line is skipped entirely (no terminator inserted)
		{token.STRING, "hi\n"},
		{token.EOF, ""},
	}

	l := New(input)
	for i, tt := range tests {
		tok := l.NextToken()
		if tok.Type != tt.wantType {
			t.Fatalf("test[%d]: type = %q, want %q (literal %q)", i, tok.Type, tt.wantType, tok.Literal)
		}
		if tok.Literal != tt.wantLiteral {
			t.Fatalf("test[%d]: literal = %q, want %q", i, tok.Literal, tt.wantLiteral)
		}
	}
}

func TestLineContinuationSuppressesTerminator(t *testing.T) {
	// A newline after a binary operator or inside () must NOT insert a terminator.
	input := "1 +\n2\nadd(\n1,\n2,\n)"
	want := []token.TokenType{
		token.INT, token.PLUS, token.INT, token.SEMICOLON,
		token.IDENT, token.LPAREN, token.INT, token.COMMA, token.INT, token.COMMA, token.RPAREN,
		token.EOF,
	}
	l := New(input)
	for i, wt := range want {
		tok := l.NextToken()
		if tok.Type != wt {
			t.Fatalf("test[%d]: type = %q, want %q", i, tok.Type, wt)
		}
	}
}

func TestTokenPositions(t *testing.T) {
	l := New("ab\n  cd")
	ab := l.NextToken()
	if ab.Span.Start.Line != 1 || ab.Span.Start.Col != 1 {
		t.Fatalf("ab start = %+v, want line 1 col 1", ab.Span.Start)
	}
	_ = l.NextToken() // inserted terminator
	cd := l.NextToken()
	if cd.Literal != "cd" {
		t.Fatalf("third token literal = %q, want cd", cd.Literal)
	}
	if cd.Span.Start.Line != 2 || cd.Span.Start.Col != 3 {
		t.Fatalf("cd start = %+v, want line 2 col 3", cd.Span.Start)
	}
}
