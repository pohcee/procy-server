package library

import (
	"regexp"
	"strings"
)

var (
	absolutePath   = regexp.MustCompile(`^(/|[A-Za-z]:\\)`)
	shellOperators = regexp.MustCompile(`[|<>;&,\n]`)
)

// NormalizeInterpreter maps the "default" spellings (including the legacy
// "shOrShebang" and "systemDefaultShell") to "" and trims anything else.
// Mirrors website/utils/interpreter.ts normalizeInterpreter.
func NormalizeInterpreter(s string) string {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "default", "shorshebang", "systemdefaultshell":
		return ""
	}
	return s
}

// ResolveKind decides whether code runs as a command or a script. It is a port
// of detectRunKind in website/utils/runKind.ts and must stay in sync with it.
func ResolveKind(code, interpreter string) Kind {
	text := strings.TrimSpace(code)
	switch NormalizeInterpreter(interpreter) {
	case "", "cmd", "powershell":
	default:
		return KindScript
	}
	if strings.HasPrefix(text, "#!") || strings.Contains(text, "\n") {
		return KindScript
	}
	if !absolutePath.MatchString(text) || shellOperators.MatchString(text) {
		return KindScript
	}
	return KindCommand
}
