package signup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApprovalMails(t *testing.T) {
	m := readyMail("https://ui.example", "Acme", "acme-x1", MethodEmail)
	assert.Contains(t, m.body, "https://ui.example/auth")
	assert.Contains(t, m.body, "the password you chose")
	m = readyMail("https://ui.example", "Acme", "acme-x1", MethodGoogle)
	assert.Contains(t, m.body, "Sign in with Google")
	assert.NotContains(t, m.body, "password")

	assert.NotContains(t, deniedMail("Acme", " ").body, "reason")
	assert.Contains(t, deniedMail("Acme", "duplicate").body, "The reason given: duplicate")

	no := false
	n := notifyMail("https://ui.example", &Signup{OrgName: "Acme", Email: "a@acme.example", Domain: "acme.example", MXFound: &no, Method: MethodGoogle}, false)
	assert.Equal(t, "New signup waiting: Acme", n.subject)
	assert.Contains(t, n.body, "Mail server:  NO")
	assert.Contains(t, n.body, "Google sign-in")
	assert.Contains(t, notifyMail("https://ui.example", &Signup{Domain: "gmail.com"}, true).body, "gmail.com (free mail)")
	assert.Contains(t, notifyMail("https://ui.example", &Signup{}, false).body, "not checked")
}

func TestIsFreeMail(t *testing.T) {
	assert.True(t, IsFreeMail("gmail.com"))
	assert.True(t, IsFreeMail("Proton.Me"))
	assert.False(t, IsFreeMail("acme.example"))
}
