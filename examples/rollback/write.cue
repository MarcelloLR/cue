// Rollback demo (DESIGN.md §7): "rollback = walk the log backward firing
// compensations". This program creates files with the reversible `fs.write` tool.
// Each successful write captures an `fs.delete(path)` compensation into the effect
// log, so a later `cue rollback <log>` can undo the run by firing those inverses in
// reverse order.
//
// Round-trip (paths under /tmp so the demo is self-contained):
//
//   # 1. Run, capturing the effect log to a JSONL file.
//   cue run examples/rollback/write.cue --log /tmp/cue-rollback.log
//   ls /tmp/cue-rollback-demo-*.txt        # the files now exist
//
//   # 2. Walk the log backward, firing each fs.write's fs.delete compensation.
//   cue rollback /tmp/cue-rollback.log --json
//   ls /tmp/cue-rollback-demo-*.txt        # gone — rollback undid the writes
//
// The rollback's own effects (the fs.delete calls) are tagged branch "rollback" in
// the §9 envelope, and a policy that denies fs.delete would block the undo with a
// CUE_CAP_* diagnostic instead of running it.

let first = fs.write("/tmp/cue-rollback-demo-1.txt", "first file")
print("wrote:", first.path)

let second = fs.write("/tmp/cue-rollback-demo-2.txt", "second file")
print("wrote:", second.path)

// The program's value is the list of paths it created.
[first.path, second.path]
