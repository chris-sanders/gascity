// Package shellquote provides POSIX shell quoting utilities.
package shellquote

import "strings"

const metacharacters = " \t\r\n\"'\\|&;$!(){}[]<>?*~#`"

// Quote returns s as a single shell-safe argument literal.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Join renders args as a shell-safe argv suffix. Simple args stay readable.
func Join(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "":
			parts = append(parts, "''")
		case strings.ContainsAny(arg, metacharacters):
			parts = append(parts, Quote(arg))
		default:
			parts = append(parts, arg)
		}
	}
	return strings.Join(parts, " ")
}

// Split parses a shell-like command string into argv.
// It is intentionally minimal but round-trips the quoting produced by Quote/Join.
func Split(command string) []string {
	var (
		args        []string
		current     strings.Builder
		inSingle    bool
		inDouble    bool
		escaped     bool
		tokenActive bool
	)

	flush := func() {
		if tokenActive || current.Len() > 0 {
			args = append(args, current.String())
			current.Reset()
			tokenActive = false
		}
	}

	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
			tokenActive = true
		case inSingle:
			if r == '\'' {
				inSingle = false
				continue
			}
			current.WriteRune(r)
			tokenActive = true
		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
				tokenActive = true
			default:
				current.WriteRune(r)
				tokenActive = true
			}
		default:
			switch r {
			case '\'':
				inSingle = true
				tokenActive = true
			case '"':
				inDouble = true
				tokenActive = true
			case '\\':
				escaped = true
				tokenActive = true
			case ' ', '\t', '\n', '\r':
				flush()
			default:
				current.WriteRune(r)
				tokenActive = true
			}
		}
	}

	if escaped {
		current.WriteRune('\\')
	}
	flush()
	return args
}
