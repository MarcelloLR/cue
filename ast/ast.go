// Package ast defines the typed node structs the parser produces and the
// evaluator walks.
//
// Every node carries a source Span (via the embedded spanned helper). This is a
// foundational decision (DESIGN.md §12): spans are information that only exists
// at parse time, so they must be captured when the node is constructed —
// retrofitting them later would mean touching every node and every parse site.
package ast

import (
	"strings"

	"github.com/MarcelloLR/cue/token"
)

// Node is the interface every AST node implements.
type Node interface {
	TokenLiteral() string
	String() string
	Span() token.Span
}

// Statement is a node that appears in statement position.
type Statement interface {
	Node
	statementNode()
}

// Expression is a node that yields a value.
type Expression interface {
	Node
	expressionNode()
}

// spanned is embedded in every concrete node to supply the Span. The Sp field
// is exported so the parser (a different package) can set it after building the
// node; the embedded type itself stays unexported.
type spanned struct {
	Sp token.Span
}

func (s *spanned) Span() token.Span { return s.Sp }

// Program is the root node: a sequence of statements.
type Program struct {
	Statements []Statement
}

func (p *Program) TokenLiteral() string {
	if len(p.Statements) > 0 {
		return p.Statements[0].TokenLiteral()
	}
	return ""
}

func (p *Program) String() string {
	var out strings.Builder
	for _, s := range p.Statements {
		out.WriteString(s.String())
	}
	return out.String()
}

func (p *Program) Span() token.Span {
	if len(p.Statements) == 0 {
		return token.Span{}
	}
	return token.Span{
		Start: p.Statements[0].Span().Start,
		End:   p.Statements[len(p.Statements)-1].Span().End,
	}
}

// --- Statements ---

// LetStatement is `let <Name> = <Value>`.
type LetStatement struct {
	spanned
	Token token.Token // the 'let' token
	Name  *Identifier
	Value Expression
}

func (ls *LetStatement) statementNode()       {}
func (ls *LetStatement) TokenLiteral() string { return ls.Token.Literal }
func (ls *LetStatement) String() string {
	var out strings.Builder
	out.WriteString("let ")
	out.WriteString(ls.Name.String())
	out.WriteString(" = ")
	if ls.Value != nil {
		out.WriteString(ls.Value.String())
	}
	return out.String()
}

// AssignStatement is `<Target> = <Value>` where Target is an existing lvalue
// (identifier, index, or member expression).
type AssignStatement struct {
	spanned
	Token  token.Token // first token of the target
	Target Expression
	Value  Expression
}

func (as *AssignStatement) statementNode()       {}
func (as *AssignStatement) TokenLiteral() string { return as.Token.Literal }
func (as *AssignStatement) String() string {
	var out strings.Builder
	out.WriteString(as.Target.String())
	out.WriteString(" = ")
	if as.Value != nil {
		out.WriteString(as.Value.String())
	}
	return out.String()
}

// ReturnStatement is `return <Value?>`.
type ReturnStatement struct {
	spanned
	Token token.Token // the 'return' token
	Value Expression  // may be nil for a bare return
}

func (rs *ReturnStatement) statementNode()       {}
func (rs *ReturnStatement) TokenLiteral() string { return rs.Token.Literal }
func (rs *ReturnStatement) String() string {
	var out strings.Builder
	out.WriteString("return")
	if rs.Value != nil {
		out.WriteString(" ")
		out.WriteString(rs.Value.String())
	}
	return out.String()
}

// ExpressionStatement is an expression used in statement position.
type ExpressionStatement struct {
	spanned
	Token token.Token // first token of the expression
	Expr  Expression
}

func (es *ExpressionStatement) statementNode()       {}
func (es *ExpressionStatement) TokenLiteral() string { return es.Token.Literal }
func (es *ExpressionStatement) String() string {
	if es.Expr != nil {
		return es.Expr.String()
	}
	return ""
}

// BlockStatement is a `{ ... }` sequence of statements.
type BlockStatement struct {
	spanned
	Token      token.Token // the '{' token
	Statements []Statement
}

func (bs *BlockStatement) statementNode()       {}
func (bs *BlockStatement) TokenLiteral() string { return bs.Token.Literal }
func (bs *BlockStatement) String() string {
	parts := make([]string, 0, len(bs.Statements))
	for _, s := range bs.Statements {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, "; ")
}

// --- Expressions ---

// Identifier is a name reference.
type Identifier struct {
	spanned
	Token token.Token
	Value string
}

func (i *Identifier) expressionNode()      {}
func (i *Identifier) TokenLiteral() string { return i.Token.Literal }
func (i *Identifier) String() string       { return i.Value }

// IntegerLiteral is a 64-bit integer literal.
type IntegerLiteral struct {
	spanned
	Token token.Token
	Value int64
}

func (il *IntegerLiteral) expressionNode()      {}
func (il *IntegerLiteral) TokenLiteral() string { return il.Token.Literal }
func (il *IntegerLiteral) String() string       { return il.Token.Literal }

// FloatLiteral is a 64-bit floating-point literal.
type FloatLiteral struct {
	spanned
	Token token.Token
	Value float64
}

func (fl *FloatLiteral) expressionNode()      {}
func (fl *FloatLiteral) TokenLiteral() string { return fl.Token.Literal }
func (fl *FloatLiteral) String() string       { return fl.Token.Literal }

// BooleanLiteral is `true` or `false`.
type BooleanLiteral struct {
	spanned
	Token token.Token
	Value bool
}

func (b *BooleanLiteral) expressionNode()      {}
func (b *BooleanLiteral) TokenLiteral() string { return b.Token.Literal }
func (b *BooleanLiteral) String() string       { return b.Token.Literal }

// NullLiteral is `null`.
type NullLiteral struct {
	spanned
	Token token.Token
}

func (n *NullLiteral) expressionNode()      {}
func (n *NullLiteral) TokenLiteral() string { return n.Token.Literal }
func (n *NullLiteral) String() string       { return "null" }

// StringLiteral is a double-quoted string. Value holds the decoded contents.
type StringLiteral struct {
	spanned
	Token token.Token
	Value string
}

func (sl *StringLiteral) expressionNode()      {}
func (sl *StringLiteral) TokenLiteral() string { return sl.Token.Literal }
func (sl *StringLiteral) String() string       { return "\"" + sl.Value + "\"" }

// PrefixExpression is `<Operator><Right>`, e.g. `-x` or `!ok`.
type PrefixExpression struct {
	spanned
	Token    token.Token
	Operator string
	Right    Expression
}

func (pe *PrefixExpression) expressionNode()      {}
func (pe *PrefixExpression) TokenLiteral() string { return pe.Token.Literal }
func (pe *PrefixExpression) String() string {
	return "(" + pe.Operator + pe.Right.String() + ")"
}

// InfixExpression is `<Left> <Operator> <Right>`.
type InfixExpression struct {
	spanned
	Token    token.Token
	Left     Expression
	Operator string
	Right    Expression
}

func (ie *InfixExpression) expressionNode()      {}
func (ie *InfixExpression) TokenLiteral() string { return ie.Token.Literal }
func (ie *InfixExpression) String() string {
	return "(" + ie.Left.String() + " " + ie.Operator + " " + ie.Right.String() + ")"
}

// IfExpression is `if <Condition> <Consequence> [else <Alternative>]`. It is an
// expression: it yields the value of whichever branch runs (or null).
type IfExpression struct {
	spanned
	Token       token.Token
	Condition   Expression
	Consequence *BlockStatement
	Alternative *BlockStatement // nil if no else; an else-if is wrapped in a block
}

func (ie *IfExpression) expressionNode()      {}
func (ie *IfExpression) TokenLiteral() string { return ie.Token.Literal }
func (ie *IfExpression) String() string {
	var out strings.Builder
	out.WriteString("if ")
	out.WriteString(ie.Condition.String())
	out.WriteString(" { ")
	out.WriteString(ie.Consequence.String())
	out.WriteString(" }")
	if ie.Alternative != nil {
		out.WriteString(" else { ")
		out.WriteString(ie.Alternative.String())
		out.WriteString(" }")
	}
	return out.String()
}

// ForExpression is `for <Var> in <Iterable> <Body>`. It evaluates to null.
type ForExpression struct {
	spanned
	Token    token.Token
	Var      *Identifier
	Iterable Expression
	Body     *BlockStatement
}

func (fe *ForExpression) expressionNode()      {}
func (fe *ForExpression) TokenLiteral() string { return fe.Token.Literal }
func (fe *ForExpression) String() string {
	var out strings.Builder
	out.WriteString("for ")
	out.WriteString(fe.Var.String())
	out.WriteString(" in ")
	out.WriteString(fe.Iterable.String())
	out.WriteString(" { ")
	out.WriteString(fe.Body.String())
	out.WriteString(" }")
	return out.String()
}

// FunctionLiteral is `fn(<Parameters>) <Body>`.
type FunctionLiteral struct {
	spanned
	Token      token.Token
	Parameters []*Identifier
	Body       *BlockStatement
}

func (fl *FunctionLiteral) expressionNode()      {}
func (fl *FunctionLiteral) TokenLiteral() string { return fl.Token.Literal }
func (fl *FunctionLiteral) String() string {
	params := make([]string, 0, len(fl.Parameters))
	for _, p := range fl.Parameters {
		params = append(params, p.String())
	}
	return "fn(" + strings.Join(params, ", ") + ") { " + fl.Body.String() + " }"
}

// CallExpression is `<Function>(<Arguments>)`.
type CallExpression struct {
	spanned
	Token     token.Token // the '(' token
	Function  Expression
	Arguments []Expression
}

func (ce *CallExpression) expressionNode()      {}
func (ce *CallExpression) TokenLiteral() string { return ce.Token.Literal }
func (ce *CallExpression) String() string {
	args := make([]string, 0, len(ce.Arguments))
	for _, a := range ce.Arguments {
		args = append(args, a.String())
	}
	return ce.Function.String() + "(" + strings.Join(args, ", ") + ")"
}

// ArrayLiteral is `[<Elements>]`.
type ArrayLiteral struct {
	spanned
	Token    token.Token // the '[' token
	Elements []Expression
}

func (al *ArrayLiteral) expressionNode()      {}
func (al *ArrayLiteral) TokenLiteral() string { return al.Token.Literal }
func (al *ArrayLiteral) String() string {
	els := make([]string, 0, len(al.Elements))
	for _, e := range al.Elements {
		els = append(els, e.String())
	}
	return "[" + strings.Join(els, ", ") + "]"
}

// HashPair is one key/value entry in a HashLiteral. Pairs are stored in source
// order so the literal round-trips deterministically.
type HashPair struct {
	Key   Expression
	Value Expression
}

// HashLiteral is `{<k>: <v>, ...}`.
type HashLiteral struct {
	spanned
	Token token.Token // the '{' token
	Pairs []HashPair
}

func (hl *HashLiteral) expressionNode()      {}
func (hl *HashLiteral) TokenLiteral() string { return hl.Token.Literal }
func (hl *HashLiteral) String() string {
	pairs := make([]string, 0, len(hl.Pairs))
	for _, p := range hl.Pairs {
		pairs = append(pairs, p.Key.String()+": "+p.Value.String())
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// IndexExpression is `<Left>[<Index>]`.
type IndexExpression struct {
	spanned
	Token token.Token // the '[' token
	Left  Expression
	Index Expression
}

func (ie *IndexExpression) expressionNode()      {}
func (ie *IndexExpression) TokenLiteral() string { return ie.Token.Literal }
func (ie *IndexExpression) String() string {
	return "(" + ie.Left.String() + "[" + ie.Index.String() + "])"
}

// MemberExpression is `<Object>.<Property>`. In Phase 0 this is sugar for
// indexing a hash with the property name; namespaced tool resolution is added
// in a later phase.
type MemberExpression struct {
	spanned
	Token        token.Token // the '.' token
	Object       Expression
	Property     string
	PropertySpan token.Span
}

func (me *MemberExpression) expressionNode()      {}
func (me *MemberExpression) TokenLiteral() string { return me.Token.Literal }
func (me *MemberExpression) String() string {
	return "(" + me.Object.String() + "." + me.Property + ")"
}
