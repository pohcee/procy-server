package chserver

import (
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/share/random"
)

// ErrClientNotConnected is dispatch.ErrClientNotConnected, kept under its old name.
var ErrClientNotConnected = dispatch.ErrClientNotConnected

// generateNewJobID makes job IDs for the API and the job runner; tests replace it.
var generateNewJobID = func() (string, error) {
	return random.UUID4()
}

// JobProvider is jobs.Provider, kept under its old name.
type JobProvider = jobs.Provider

// libraryServerURL is the public server URL filled into built-in entries (see jobrunner.LibraryServerURL).
func (al *APIListener) libraryServerURL() string {
	if al.Server == nil {
		return ""
	}
	return jobrunner.LibraryServerURL(al.config)
}

// libraryServeVars are the placeholder values filled into built-in entries the
// API serves; nil when the server URL is unavailable (placeholder stays).
func (al *APIListener) libraryServeVars() map[string]string {
	if u := al.libraryServerURL(); u != "" {
		return map[string]string{"server_url": u}
	}
	return nil
}
