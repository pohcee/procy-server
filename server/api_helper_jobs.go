package chserver

import (
	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/jobrunner"
	"github.com/pohcee/procy-server/share/random"
)

// generateNewJobID makes the job IDs the API assigns (single-client jobs, websocket runs); tests
// replace it, and the test wiring also gives it to the job runner.
var generateNewJobID = func() (string, error) {
	return random.UUID4()
}

// JobProvider is jobs.Provider, kept under its old name.
type JobProvider = jobs.Provider

// libraryServerURL is the public server URL filled into built-in entries (see jobrunner.LibraryServerURL).
func (al *APIListener) libraryServerURL() string {
	if al.apiServices == nil {
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
