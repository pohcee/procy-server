package clientconfig

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Servers built with Go < 1.21 cannot decode regexp.Regexp encoded by newer Go (a string),
// and reject the whole connection request. The compiled regexps must not be sent.
func TestCommandsConfigJSONOmitsCompiledRegexps(t *testing.T) {
	c := Config{}
	c.RemoteCommands.Allow = []string{".*"}
	c.RemoteCommands.AllowRegexp = []*regexp.Regexp{regexp.MustCompile(".*")}
	c.RemoteCommands.DenyRegexp = []*regexp.Regexp{regexp.MustCompile("rm")}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "allow_regexp") || strings.Contains(s, "deny_regexp") {
		t.Fatalf("compiled regexps must not be serialized: %s", s)
	}
	if !strings.Contains(s, `"allow":[".*"]`) {
		t.Fatalf("allow list must still be sent: %s", s)
	}
}
