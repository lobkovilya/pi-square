package harness

import "strings"

// ShellJoin quotes argv for bash. Raw shell programs should not use it.
func ShellJoin(argv ...string) string {
	parts := make([]string, len(argv))
	for i, value := range argv {
		parts[i] = "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
	}
	return strings.Join(parts, " ")
}
