// Package lexer turns Cue source text into a stream of tokens.
//
// Beyond classic scanning it implements Go-style implicit statement
// termination (DESIGN.md §2): a newline ends a statement, except when the
// previous token cannot end one (so binary operators, commas, and open
// brackets act as line continuations) or when nesting inside () or [].
package lexer

import (
	"strings"

	"github.com/MarcelloLR/cue/token"
)

// Lexer scans an input string.
type Lexer struct {
	input string

	pos     int  // index of the current char (ch)
	readPos int  // index of the next char
	ch      byte // current char under examination (0 == EOF)

	line int // 1-based line of ch
	col  int // 1-based column of ch

	lastType token.TokenType // type of the previously emitted token
	depth    int             // nesting depth inside () and [] (suppresses newline termination)
}

// New returns a Lexer positioned at the first character of input.
func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1, col: 0}
	l.readChar()
	return l
}

// readChar advances one byte, maintaining line/column. The column resets when
// we move past a newline.
func (l *Lexer) readChar() {
	if l.ch == '\n' {
		l.line++
		l.col = 0
	}
	if l.readPos >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPos]
	}
	l.pos = l.readPos
	l.readPos++
	l.col++
}

func (l *Lexer) peekChar() byte {
	if l.readPos >= len(l.input) {
		return 0
	}
	return l.input[l.readPos]
}

func (l *Lexer) curPos() token.Position {
	return token.Position{Line: l.line, Col: l.col, Offset: l.pos}
}

// NextToken returns the next token, inserting a synthetic SEMICOLON terminator
// where the newline rule calls for one.
func (l *Lexer) NextToken() token.Token {
	if tok, ok := l.skipTrivia(); ok {
		l.lastType = tok.Type
		return tok
	}

	start := l.curPos()

	switch l.ch {
	case 0:
		return l.emit(token.EOF, 0, start)
	case '=':
		if l.peekChar() == '=' {
			return l.emit(token.EQ, 2, start)
		}
		return l.emit(token.ASSIGN, 1, start)
	case '+':
		return l.emit(token.PLUS, 1, start)
	case '-':
		return l.emit(token.MINUS, 1, start)
	case '*':
		return l.emit(token.ASTERISK, 1, start)
	case '/':
		return l.emit(token.SLASH, 1, start)
	case '%':
		return l.emit(token.PERCENT, 1, start)
	case '!':
		if l.peekChar() == '=' {
			return l.emit(token.NOT_EQ, 2, start)
		}
		return l.emit(token.BANG, 1, start)
	case '<':
		if l.peekChar() == '=' {
			return l.emit(token.LE, 2, start)
		}
		return l.emit(token.LT, 1, start)
	case '>':
		if l.peekChar() == '=' {
			return l.emit(token.GE, 2, start)
		}
		return l.emit(token.GT, 1, start)
	case '&':
		if l.peekChar() == '&' {
			return l.emit(token.AND, 2, start)
		}
		return l.emit(token.ILLEGAL, 1, start)
	case '|':
		if l.peekChar() == '|' {
			return l.emit(token.OR, 2, start)
		}
		return l.emit(token.ILLEGAL, 1, start)
	case '.':
		return l.emit(token.DOT, 1, start)
	case ',':
		return l.emit(token.COMMA, 1, start)
	case ':':
		return l.emit(token.COLON, 1, start)
	case ';':
		return l.emit(token.SEMICOLON, 1, start)
	case '(':
		l.depth++
		return l.emit(token.LPAREN, 1, start)
	case ')':
		if l.depth > 0 {
			l.depth--
		}
		return l.emit(token.RPAREN, 1, start)
	case '[':
		l.depth++
		return l.emit(token.LBRACKET, 1, start)
	case ']':
		if l.depth > 0 {
			l.depth--
		}
		return l.emit(token.RBRACKET, 1, start)
	case '{':
		return l.emit(token.LBRACE, 1, start)
	case '}':
		return l.emit(token.RBRACE, 1, start)
	case '"':
		return l.readString(start)
	default:
		if isLetter(l.ch) {
			return l.readIdentifier(start)
		}
		if isDigit(l.ch) {
			return l.readNumber(start)
		}
		return l.emit(token.ILLEGAL, 1, start)
	}
}

// emit consumes n characters and returns a token of type t spanning them.
func (l *Lexer) emit(t token.TokenType, n int, start token.Position) token.Token {
	for i := 0; i < n; i++ {
		l.readChar()
	}
	end := l.curPos()
	lit := ""
	if start.Offset < end.Offset {
		lit = l.input[start.Offset:end.Offset]
	}
	tok := token.Token{Type: t, Literal: lit, Span: token.Span{Start: start, End: end}}
	l.lastType = t
	return tok
}

func (l *Lexer) readIdentifier(start token.Position) token.Token {
	for isLetter(l.ch) || isDigit(l.ch) {
		l.readChar()
	}
	end := l.curPos()
	lit := l.input[start.Offset:end.Offset]
	t := token.LookupIdent(lit)
	tok := token.Token{Type: t, Literal: lit, Span: token.Span{Start: start, End: end}}
	l.lastType = t
	return tok
}

func (l *Lexer) readNumber(start token.Position) token.Token {
	for isDigit(l.ch) {
		l.readChar()
	}
	isFloat := false
	if l.ch == '.' && isDigit(l.peekChar()) {
		isFloat = true
		l.readChar() // consume '.'
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	end := l.curPos()
	lit := l.input[start.Offset:end.Offset]
	t := token.INT
	if isFloat {
		t = token.FLOAT
	}
	tok := token.Token{Type: t, Literal: lit, Span: token.Span{Start: start, End: end}}
	l.lastType = t
	return tok
}

// readString scans a double-quoted string, decoding escapes. The Literal holds
// the decoded value (without quotes). An unterminated string yields ILLEGAL.
func (l *Lexer) readString(start token.Position) token.Token {
	l.readChar() // consume opening quote
	var sb strings.Builder
	for l.ch != '"' && l.ch != 0 && l.ch != '\n' {
		if l.ch == '\\' {
			l.readChar()
			switch l.ch {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case 0:
				// trailing backslash at EOF; fall through to unterminated
			default:
				sb.WriteByte(l.ch)
			}
			if l.ch != 0 {
				l.readChar()
			}
			continue
		}
		sb.WriteByte(l.ch)
		l.readChar()
	}
	terminated := l.ch == '"'
	if terminated {
		l.readChar() // consume closing quote
	}
	end := l.curPos()
	t := token.STRING
	if !terminated {
		t = token.ILLEGAL
	}
	tok := token.Token{Type: t, Literal: sb.String(), Span: token.Span{Start: start, End: end}}
	l.lastType = t
	return tok
}

// skipTrivia advances over spaces, comments, and newlines. If a newline should
// terminate the current statement, it returns a synthetic SEMICOLON and true.
func (l *Lexer) skipTrivia() (token.Token, bool) {
	for {
		switch {
		case l.ch == ' ' || l.ch == '\t' || l.ch == '\r':
			l.readChar()
		case l.ch == '\n':
			if l.depth == 0 && terminates(l.lastType) {
				start := l.curPos()
				l.readChar()
				end := l.curPos()
				return token.Token{Type: token.SEMICOLON, Literal: "\n", Span: token.Span{Start: start, End: end}}, true
			}
			l.readChar()
		case l.ch == '/' && l.peekChar() == '/':
			for l.ch != '\n' && l.ch != 0 {
				l.readChar()
			}
		case l.ch == '/' && l.peekChar() == '*':
			l.readChar()
			l.readChar()
			for !(l.ch == '*' && l.peekChar() == '/') && l.ch != 0 {
				l.readChar()
			}
			if l.ch != 0 {
				l.readChar()
				l.readChar()
			}
		default:
			return token.Token{}, false
		}
	}
}

// terminates reports whether a token of type t can end a statement, in which
// case a following newline inserts a terminator.
func terminates(t token.TokenType) bool {
	switch t {
	case token.IDENT, token.INT, token.FLOAT, token.STRING,
		token.TRUE, token.FALSE, token.NULL,
		token.RPAREN, token.RBRACKET, token.RBRACE,
		token.RETURN:
		return true
	}
	return false
}

func isLetter(ch byte) bool {
	return ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}
