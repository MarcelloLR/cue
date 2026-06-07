package object

// Environment is a lexical scope: a set of name→value bindings plus an optional
// enclosing scope. Closures capture the Environment they were defined in.
type Environment struct {
	store map[string]Object
	outer *Environment
}

// NewEnvironment returns an empty top-level scope.
func NewEnvironment() *Environment {
	return &Environment{store: map[string]Object{}}
}

// NewEnclosedEnvironment returns a child scope nested inside outer.
func NewEnclosedEnvironment(outer *Environment) *Environment {
	e := NewEnvironment()
	e.outer = outer
	return e
}

// Get looks up a name, walking outward through enclosing scopes.
func (e *Environment) Get(name string) (Object, bool) {
	if obj, ok := e.store[name]; ok {
		return obj, true
	}
	if e.outer != nil {
		return e.outer.Get(name)
	}
	return nil, false
}

// Set binds name in this scope, introducing a new binding (used by `let`).
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}

// Assign updates an existing binding in the nearest enclosing scope that
// defines it. It reports false if name is unbound (used by `=` reassignment,
// which must not create new bindings).
func (e *Environment) Assign(name string, val Object) bool {
	if _, ok := e.store[name]; ok {
		e.store[name] = val
		return true
	}
	if e.outer != nil {
		return e.outer.Assign(name, val)
	}
	return false
}
