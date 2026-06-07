// Package parser turns a token stream into an AST using recursive descent for
// statements and Pratt (precedence-climbing) parsing for expressions.
//
// Rather than aborting on the first error, it records structured diagnostics
// into a diag.Collector and recovers at statement boundaries, so one pass can
// report many problems (DESIGN.md §9).
package parser

import (
	"strconv"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/token"
)

// Precedence levels, lowest to highest.
const (
	_ int = iota
	LOWEST
	OR          // ||
	AND         // &&
	EQUALS      // == !=
	LESSGREATER // < > <= >=
	SUM         // + -
	PRODUCT     // * / %
	PREFIX      // -x !x
	CALL        // foo(...)  a[i]  a.b
)

var precedences = map[token.TokenType]int{
	token.OR:       OR,
	token.AND:      AND,
	token.EQ:       EQUALS,
	token.NOT_EQ:   EQUALS,
	token.LT:       LESSGREATER,
	token.GT:       LESSGREATER,
	token.LE:       LESSGREATER,
	token.GE:       LESSGREATER,
	token.PLUS:     SUM,
	token.MINUS:    SUM,
	token.SLASH:    PRODUCT,
	token.ASTERISK: PRODUCT,
	token.PERCENT:  PRODUCT,
	token.LPAREN:   CALL,
	token.LBRACKET: CALL,
	token.DOT:      CALL,
}

type (
	prefixParseFn func() ast.Expression
	infixParseFn  func(ast.Expression) ast.Expression
)

// Parser holds the lexer, two-token lookahead, the diagnostic collector, and
// the prefix/infix dispatch tables.
type Parser struct {
	l     *lexer.Lexer
	diags *diag.Collector

	cur  token.Token
	peek token.Token

	prefixFns map[token.TokenType]prefixParseFn
	infixFns  map[token.TokenType]infixParseFn
}

// New returns a Parser ready to ParseProgram.
func New(l *lexer.Lexer) *Parser {
	p := &Parser{l: l, diags: &diag.Collector{}}

	p.prefixFns = map[token.TokenType]prefixParseFn{
		token.IDENT:    p.parseIdentifier,
		token.INT:      p.parseIntegerLiteral,
		token.FLOAT:    p.parseFloatLiteral,
		token.STRING:   p.parseStringLiteral,
		token.TRUE:     p.parseBooleanLiteral,
		token.FALSE:    p.parseBooleanLiteral,
		token.NULL:     p.parseNullLiteral,
		token.BANG:     p.parsePrefixExpression,
		token.MINUS:    p.parsePrefixExpression,
		token.LPAREN:   p.parseGroupedExpression,
		token.IF:       p.parseIfExpression,
		token.FOR:      p.parseForExpression,
		token.PARALLEL: p.parseParallelExpression,
		token.RETRY:    p.parseRetryExpression,
		token.FN:       p.parseFunctionLiteral,
		token.LBRACKET: p.parseArrayLiteral,
		token.LBRACE:   p.parseHashLiteral,
	}
	p.infixFns = map[token.TokenType]infixParseFn{
		token.PLUS:     p.parseInfixExpression,
		token.MINUS:    p.parseInfixExpression,
		token.ASTERISK: p.parseInfixExpression,
		token.SLASH:    p.parseInfixExpression,
		token.PERCENT:  p.parseInfixExpression,
		token.EQ:       p.parseInfixExpression,
		token.NOT_EQ:   p.parseInfixExpression,
		token.LT:       p.parseInfixExpression,
		token.GT:       p.parseInfixExpression,
		token.LE:       p.parseInfixExpression,
		token.GE:       p.parseInfixExpression,
		token.AND:      p.parseInfixExpression,
		token.OR:       p.parseInfixExpression,
		token.LPAREN:   p.parseCallExpression,
		token.LBRACKET: p.parseIndexExpression,
		token.DOT:      p.parseMemberExpression,
	}

	// Prime cur and peek.
	p.nextToken()
	p.nextToken()
	return p
}

// Diagnostics returns the diagnostics collected so far.
func (p *Parser) Diagnostics() []diag.Diagnostic { return p.diags.Items() }

// HasErrors reports whether any error-severity diagnostic was collected.
func (p *Parser) HasErrors() bool { return p.diags.HasErrors() }

func (p *Parser) nextToken() {
	p.cur = p.peek
	p.peek = p.l.NextToken()
	if p.peek.Type == token.ILLEGAL {
		p.diags.Error(diag.LexIllegal, p.peek.Span, "illegal token %q", p.peek.Literal)
	}
}

func (p *Parser) curPrecedence() int {
	if pr, ok := precedences[p.cur.Type]; ok {
		return pr
	}
	return LOWEST
}

func (p *Parser) peekPrecedence() int {
	if pr, ok := precedences[p.peek.Type]; ok {
		return pr
	}
	return LOWEST
}

func (p *Parser) expectPeek(t token.TokenType) bool {
	if p.peek.Type == t {
		p.nextToken()
		return true
	}
	p.diags.Error(diag.ParseUnexpectedToken, p.peek.Span, "expected %s, found %q", t, p.peek.Literal)
	return false
}

// spanFrom builds a span from a starting token through the current token.
func (p *Parser) spanFrom(start token.Token) token.Span {
	return token.Span{Start: start.Span.Start, End: p.cur.Span.End}
}

// ParseProgram parses the whole input into a Program node.
func (p *Parser) ParseProgram() *ast.Program {
	prog := &ast.Program{}
	for p.cur.Type != token.EOF {
		if p.cur.Type == token.SEMICOLON {
			p.nextToken()
			continue
		}
		stmt := p.parseStatement()
		if stmt != nil {
			prog.Statements = append(prog.Statements, stmt)
		}
		p.nextToken()
	}
	return prog
}

func (p *Parser) parseStatement() ast.Statement {
	switch p.cur.Type {
	case token.LET:
		return p.parseLetStatement()
	case token.RETURN:
		return p.parseReturnStatement()
	default:
		return p.parseExpressionOrAssign()
	}
}

func (p *Parser) parseLetStatement() ast.Statement {
	start := p.cur
	if !p.expectPeek(token.IDENT) {
		return nil
	}
	name := &ast.Identifier{Token: p.cur, Value: p.cur.Literal}
	name.Sp = p.cur.Span
	if !p.expectPeek(token.ASSIGN) {
		return nil
	}
	p.nextToken()
	value := p.parseExpression(LOWEST)
	stmt := &ast.LetStatement{Token: start, Name: name, Value: value}
	stmt.Sp = p.spanFrom(start)
	p.consumeOptionalTerminator()
	return stmt
}

func (p *Parser) parseReturnStatement() ast.Statement {
	start := p.cur
	stmt := &ast.ReturnStatement{Token: start}
	if p.peek.Type == token.SEMICOLON || p.peek.Type == token.EOF || p.peek.Type == token.RBRACE {
		stmt.Sp = p.spanFrom(start)
		p.consumeOptionalTerminator()
		return stmt
	}
	p.nextToken()
	stmt.Value = p.parseExpression(LOWEST)
	stmt.Sp = p.spanFrom(start)
	p.consumeOptionalTerminator()
	return stmt
}

// parseExpressionOrAssign parses a statement that starts with an expression. If
// the expression is followed by '=', it becomes an assignment.
func (p *Parser) parseExpressionOrAssign() ast.Statement {
	start := p.cur
	expr := p.parseExpression(LOWEST)
	if expr == nil {
		return nil
	}

	if p.peek.Type == token.ASSIGN {
		if !isLValue(expr) {
			p.diags.Error(diag.ParseInvalidAssign, expr.Span(), "cannot assign to this expression")
		}
		p.nextToken() // cur = '='
		p.nextToken() // cur = first token of value
		value := p.parseExpression(LOWEST)
		stmt := &ast.AssignStatement{Token: start, Target: expr, Value: value}
		stmt.Sp = p.spanFrom(start)
		p.consumeOptionalTerminator()
		return stmt
	}

	stmt := &ast.ExpressionStatement{Token: start, Expr: expr}
	stmt.Sp = p.spanFrom(start)
	p.consumeOptionalTerminator()
	return stmt
}

func (p *Parser) consumeOptionalTerminator() {
	if p.peek.Type == token.SEMICOLON {
		p.nextToken()
	}
}

func isLValue(e ast.Expression) bool {
	switch e.(type) {
	case *ast.Identifier, *ast.IndexExpression, *ast.MemberExpression:
		return true
	}
	return false
}

// parseExpression is the Pratt loop.
func (p *Parser) parseExpression(prec int) ast.Expression {
	prefix := p.prefixFns[p.cur.Type]
	if prefix == nil {
		code := diag.ParseNoPrefix
		if p.cur.Type == token.ILLEGAL {
			code = diag.LexIllegal
		}
		p.diags.Error(code, p.cur.Span, "unexpected %q in expression", p.cur.Literal)
		return nil
	}
	left := prefix()

	for p.peek.Type != token.SEMICOLON && prec < p.peekPrecedence() {
		infix := p.infixFns[p.peek.Type]
		if infix == nil {
			return left
		}
		p.nextToken()
		left = infix(left)
	}
	return left
}

// --- prefix parse functions ---

func (p *Parser) parseIdentifier() ast.Expression {
	id := &ast.Identifier{Token: p.cur, Value: p.cur.Literal}
	id.Sp = p.cur.Span
	return id
}

func (p *Parser) parseIntegerLiteral() ast.Expression {
	v, err := strconv.ParseInt(p.cur.Literal, 10, 64)
	if err != nil {
		p.diags.Error(diag.ParseInvalidNumber, p.cur.Span, "invalid integer literal %q", p.cur.Literal)
		return nil
	}
	lit := &ast.IntegerLiteral{Token: p.cur, Value: v}
	lit.Sp = p.cur.Span
	return lit
}

func (p *Parser) parseFloatLiteral() ast.Expression {
	v, err := strconv.ParseFloat(p.cur.Literal, 64)
	if err != nil {
		p.diags.Error(diag.ParseInvalidNumber, p.cur.Span, "invalid float literal %q", p.cur.Literal)
		return nil
	}
	lit := &ast.FloatLiteral{Token: p.cur, Value: v}
	lit.Sp = p.cur.Span
	return lit
}

func (p *Parser) parseStringLiteral() ast.Expression {
	lit := &ast.StringLiteral{Token: p.cur, Value: p.cur.Literal}
	lit.Sp = p.cur.Span
	return lit
}

func (p *Parser) parseBooleanLiteral() ast.Expression {
	lit := &ast.BooleanLiteral{Token: p.cur, Value: p.cur.Type == token.TRUE}
	lit.Sp = p.cur.Span
	return lit
}

func (p *Parser) parseNullLiteral() ast.Expression {
	lit := &ast.NullLiteral{Token: p.cur}
	lit.Sp = p.cur.Span
	return lit
}

func (p *Parser) parsePrefixExpression() ast.Expression {
	start := p.cur
	expr := &ast.PrefixExpression{Token: start, Operator: start.Literal}
	p.nextToken()
	expr.Right = p.parseExpression(PREFIX)
	expr.Sp = p.spanFrom(start)
	return expr
}

func (p *Parser) parseGroupedExpression() ast.Expression {
	p.nextToken()
	exp := p.parseExpression(LOWEST)
	if !p.expectPeek(token.RPAREN) {
		return nil
	}
	return exp
}

func (p *Parser) parseIfExpression() ast.Expression {
	start := p.cur
	expr := &ast.IfExpression{Token: start}
	p.nextToken()
	expr.Condition = p.parseExpression(LOWEST)
	if !p.expectPeek(token.LBRACE) {
		return nil
	}
	expr.Consequence = p.parseBlockStatement()

	if p.peek.Type == token.ELSE {
		p.nextToken() // cur = else
		if p.peek.Type == token.IF {
			p.nextToken() // cur = if
			elseIf := p.parseIfExpression()
			es := &ast.ExpressionStatement{Token: p.cur, Expr: elseIf}
			blk := &ast.BlockStatement{Token: p.cur, Statements: []ast.Statement{es}}
			if elseIf != nil {
				es.Sp = elseIf.Span()
				blk.Sp = elseIf.Span()
			}
			expr.Alternative = blk
		} else {
			if !p.expectPeek(token.LBRACE) {
				return nil
			}
			expr.Alternative = p.parseBlockStatement()
		}
	}
	expr.Sp = p.spanFrom(start)
	return expr
}

func (p *Parser) parseForExpression() ast.Expression {
	start := p.cur
	fe := &ast.ForExpression{Token: start}
	if !p.expectPeek(token.IDENT) {
		return nil
	}
	fe.Var = &ast.Identifier{Token: p.cur, Value: p.cur.Literal}
	fe.Var.Sp = p.cur.Span
	if !p.expectPeek(token.IN) {
		return nil
	}
	p.nextToken()
	fe.Iterable = p.parseExpression(LOWEST)
	if !p.expectPeek(token.LBRACE) {
		return nil
	}
	fe.Body = p.parseBlockStatement()
	fe.Sp = p.spanFrom(start)
	return fe
}

// parseParallelExpression parses the parallel map form
// `parallel ( IDENT in EXPR [ , limit = EXPR ] ) BLOCK` (DESIGN.md §3, §6).
// The leading '(' opens the lexer's paren-depth, so newlines inside the head are
// line continuations. The optional `limit = EXPR` caps concurrency; absent, the
// evaluator's default limit applies. The block form (`parallel BLOCK`) is
// phase-later and is not parsed here.
func (p *Parser) parseParallelExpression() ast.Expression {
	start := p.cur
	pe := &ast.ParallelExpression{Token: start}
	if !p.expectPeek(token.LPAREN) {
		return nil
	}
	if !p.expectPeek(token.IDENT) {
		return nil
	}
	pe.Var = &ast.Identifier{Token: p.cur, Value: p.cur.Literal}
	pe.Var.Sp = p.cur.Span
	if !p.expectPeek(token.IN) {
		return nil
	}
	p.nextToken()
	pe.Iterable = p.parseExpression(LOWEST)

	// Optional inline bound: `, limit = EXPR`.
	if p.peek.Type == token.COMMA {
		p.nextToken() // cur = ','
		if !p.expectPeek(token.IDENT) || p.cur.Literal != "limit" {
			p.diags.Error(diag.ParseUnexpectedToken, p.cur.Span,
				"expected `limit`, found %q", p.cur.Literal)
			return nil
		}
		if !p.expectPeek(token.ASSIGN) {
			return nil
		}
		p.nextToken()
		pe.Limit = p.parseExpression(LOWEST)
	}

	if !p.expectPeek(token.RPAREN) {
		return nil
	}
	if !p.expectPeek(token.LBRACE) {
		return nil
	}
	pe.Body = p.parseBlockStatement()
	pe.Sp = p.spanFrom(start)
	return pe
}

// parseRetryExpression parses the retry form `retry ( EXPR ) BLOCK` (DESIGN.md
// §3, §8), mirroring parseParallelExpression: the leading '(' opens the lexer's
// paren-depth so newlines inside the head are line continuations, and the body is
// an ordinary block. EXPR is the maximum attempt count, evaluated and validated
// (positive integer) at runtime. Like `parallel`, retry is an expression.
func (p *Parser) parseRetryExpression() ast.Expression {
	start := p.cur
	re := &ast.RetryExpression{Token: start}
	if !p.expectPeek(token.LPAREN) {
		return nil
	}
	p.nextToken()
	re.Attempts = p.parseExpression(LOWEST)
	if !p.expectPeek(token.RPAREN) {
		return nil
	}
	if !p.expectPeek(token.LBRACE) {
		return nil
	}
	re.Body = p.parseBlockStatement()
	re.Sp = p.spanFrom(start)
	return re
}

func (p *Parser) parseFunctionLiteral() ast.Expression {
	start := p.cur
	lit := &ast.FunctionLiteral{Token: start}
	if !p.expectPeek(token.LPAREN) {
		return nil
	}
	lit.Parameters = p.parseFunctionParameters()
	if !p.expectPeek(token.LBRACE) {
		return nil
	}
	lit.Body = p.parseBlockStatement()
	lit.Sp = p.spanFrom(start)
	return lit
}

func (p *Parser) parseFunctionParameters() []*ast.Identifier {
	var params []*ast.Identifier
	if p.peek.Type == token.RPAREN {
		p.nextToken()
		return params
	}
	p.nextToken()
	first := &ast.Identifier{Token: p.cur, Value: p.cur.Literal}
	first.Sp = p.cur.Span
	params = append(params, first)
	for p.peek.Type == token.COMMA {
		p.nextToken()
		p.nextToken()
		id := &ast.Identifier{Token: p.cur, Value: p.cur.Literal}
		id.Sp = p.cur.Span
		params = append(params, id)
	}
	if !p.expectPeek(token.RPAREN) {
		return nil
	}
	return params
}

func (p *Parser) parseBlockStatement() *ast.BlockStatement {
	start := p.cur // '{'
	block := &ast.BlockStatement{Token: start}
	p.nextToken()
	for p.cur.Type != token.RBRACE && p.cur.Type != token.EOF {
		if p.cur.Type == token.SEMICOLON {
			p.nextToken()
			continue
		}
		stmt := p.parseStatement()
		if stmt != nil {
			block.Statements = append(block.Statements, stmt)
		}
		p.nextToken()
	}
	if p.cur.Type != token.RBRACE {
		p.diags.Error(diag.ParseUnexpectedToken, p.cur.Span, "expected }, found %q", p.cur.Literal)
	}
	block.Sp = token.Span{Start: start.Span.Start, End: p.cur.Span.End}
	return block
}

// --- infix parse functions ---

func (p *Parser) parseInfixExpression(left ast.Expression) ast.Expression {
	op := p.cur
	expr := &ast.InfixExpression{Token: op, Operator: op.Literal, Left: left}
	prec := p.curPrecedence()
	p.nextToken()
	expr.Right = p.parseExpression(prec)
	expr.Sp = token.Span{Start: left.Span().Start, End: p.cur.Span.End}
	return expr
}

func (p *Parser) parseCallExpression(fn ast.Expression) ast.Expression {
	call := &ast.CallExpression{Token: p.cur, Function: fn}
	call.Arguments = p.parseExpressionList(token.RPAREN)
	call.Sp = token.Span{Start: fn.Span().Start, End: p.cur.Span.End}
	return call
}

func (p *Parser) parseIndexExpression(left ast.Expression) ast.Expression {
	start := p.cur // '['
	idx := &ast.IndexExpression{Token: start, Left: left}
	p.nextToken()
	idx.Index = p.parseExpression(LOWEST)
	if !p.expectPeek(token.RBRACKET) {
		return nil
	}
	idx.Sp = token.Span{Start: left.Span().Start, End: p.cur.Span.End}
	return idx
}

func (p *Parser) parseMemberExpression(left ast.Expression) ast.Expression {
	dot := p.cur
	if !p.expectPeek(token.IDENT) {
		return nil
	}
	m := &ast.MemberExpression{
		Token:        dot,
		Object:       left,
		Property:     p.cur.Literal,
		PropertySpan: p.cur.Span,
	}
	m.Sp = token.Span{Start: left.Span().Start, End: p.cur.Span.End}
	return m
}

func (p *Parser) parseArrayLiteral() ast.Expression {
	start := p.cur
	arr := &ast.ArrayLiteral{Token: start}
	arr.Elements = p.parseExpressionList(token.RBRACKET)
	arr.Sp = p.spanFrom(start)
	return arr
}

func (p *Parser) parseHashLiteral() ast.Expression {
	start := p.cur
	hash := &ast.HashLiteral{Token: start}
	for p.peek.Type != token.RBRACE {
		p.nextToken()
		key := p.parseExpression(LOWEST)
		if !p.expectPeek(token.COLON) {
			return nil
		}
		p.nextToken()
		value := p.parseExpression(LOWEST)
		hash.Pairs = append(hash.Pairs, ast.HashPair{Key: key, Value: value})
		if p.peek.Type != token.RBRACE && !p.expectPeek(token.COMMA) {
			return nil
		}
	}
	if !p.expectPeek(token.RBRACE) {
		return nil
	}
	hash.Sp = p.spanFrom(start)
	return hash
}

// parseExpressionList parses a comma-separated list terminated by end.
func (p *Parser) parseExpressionList(end token.TokenType) []ast.Expression {
	var list []ast.Expression
	if p.peek.Type == end {
		p.nextToken()
		return list
	}
	p.nextToken()
	list = append(list, p.parseExpression(LOWEST))
	for p.peek.Type == token.COMMA {
		p.nextToken()
		p.nextToken()
		list = append(list, p.parseExpression(LOWEST))
	}
	if !p.expectPeek(end) {
		return nil
	}
	return list
}
