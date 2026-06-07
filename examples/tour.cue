// A tour of Cue's Phase 0 language core.
// Run with:  cue run examples/tour.cue

// let bindings, ints, floats, and operator precedence.
let answer = 2 + 3 * 4          // 14, not 20
print("answer:", answer)
print("float:", 10 / 4.0)       // int/float coercion -> 2.5

// Strings and booleans.
let name = "Cue"
print("hello, " + name)
print("truthy:", !!name, 1 < 2 && 2 < 3)

// Closures and recursion.
let fib = fn(n) {
    if n < 2 {
        return n
    }
    return fib(n - 1) + fib(n - 2)
}
print("fib(10):", fib(10))      // 55

// Higher-order functions.
let apply = fn(f, x) { f(x) }
print("apply double:", apply(fn(x) { x * 2 }, 21))

// Arrays, indexing, and builtins.
let xs = [1, 2, 3, 4]
print("len:", len(xs), "first:", first(xs), "last:", last(xs))
let ys = push(xs, 5)
print("pushed:", ys)

// for-loops and mutable reassignment.
let total = 0
for x in xs {
    total = total + x
}
print("sum:", total)            // 10

// Counted loop via range.
let squares = []
for i in range(5) {
    squares = push(squares, i * i)
}
print("squares:", squares)      // [0, 1, 4, 9, 16]

// Hashes and member access (sugar for ["..."]).
let user = {"name": "Ada", "age": 36}
print("name:", user.name, "age:", user["age"])
user.age = 37
print("birthday:", user.age)

// Iterating a hash yields its keys.
for k in user {
    print("field:", k, "=", str(user[k]))
}
