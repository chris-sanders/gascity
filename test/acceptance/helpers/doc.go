// Package acceptancehelpers provides a DSL for acceptance tests that exercise
// the real gc binary end-to-end. Tests use this package to init cities, start
// agents, inspect environment variables, dispatch work, and verify lifecycle
// behavior — all through the CLI, never through internal function calls.
//
// The DSL follows Dave Farley's four-layer model:
//
//	Test Cases → DSL (this package) → Protocol Driver (gc binary) → System
//
// All methods shell out to the real gc binary built in TestMain. The City
// struct carries the isolated environment (GC_HOME, XDG_RUNTIME_DIR) so tests
// cannot pollute the host's supervisor registry or tmux sessions.
package acceptancehelpers
