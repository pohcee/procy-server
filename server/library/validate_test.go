package library

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apierrors "github.com/openrport/openrport/server/api/errors"
)

func validContent() Content {
	return Content{
		Name:            "Uptime",
		Implementations: map[string]Implementation{OSLinux: {Kind: KindAuto, Code: "/usr/bin/uptime", TimeoutSec: 60}},
	}
}

// requireAPIError400 fails the test unless err is an APIError with status 400.
func requireAPIError400(t *testing.T, err error) {
	t.Helper()
	_ = apiError400(t, err)
}

// apiError400 is requireAPIError400 that also returns the APIError.
func apiError400(t *testing.T, err error) apierrors.APIError {
	t.Helper()
	var apiErr apierrors.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", apiErr.HTTPStatus)
	}
	return apiErr
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := Validate(validContent()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsEmptyName(t *testing.T) {
	c := validContent()
	c.Name = "  "
	requireAPIError400(t, Validate(c))
}

func TestValidateRejectsUnknownOS(t *testing.T) {
	c := validContent()
	c.Implementations["macos"] = Implementation{Code: "/bin/ls"}
	apiErr := apiError400(t, Validate(c))
	if !strings.Contains(apiErr.Message, "macos") {
		t.Errorf("message %q does not mention macos", apiErr.Message)
	}
}

func TestValidateRequiresImplementation(t *testing.T) {
	c := validContent()
	c.Implementations = nil
	requireAPIError400(t, Validate(c))
}

func TestValidateRejectsEmptyCode(t *testing.T) {
	c := validContent()
	c.Implementations[OSLinux] = Implementation{Code: "   "}
	requireAPIError400(t, Validate(c))
}

func TestValidateRejectsNegativeTimeout(t *testing.T) {
	c := validContent()
	c.Implementations[OSLinux] = Implementation{Code: "/bin/ls", TimeoutSec: -1}
	requireAPIError400(t, Validate(c))
}

func TestValidateRejectsBadInterpreterForCommand(t *testing.T) {
	c := validContent()
	// a command (absolute path, one line) with the script-only tacoscript interpreter
	c.Implementations[OSWindows] = Implementation{Kind: KindCommand, Code: "/bin/ls", Interpreter: "tacoscript"}
	apiErr := apiError400(t, Validate(c))
	if !strings.Contains(apiErr.Message, OSWindows) {
		t.Errorf("message %q does not name the OS", apiErr.Message)
	}
}

func TestValidateAllowsCustomInterpreterForScript(t *testing.T) {
	c := validContent()
	c.Implementations[OSLinux] = Implementation{Kind: KindScript, Code: "echo hi", Interpreter: "/bin/bash"}
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
}

func TestChangedListsOSAndFields(t *testing.T) {
	old := validContent()
	old.Implementations[OSWindows] = Implementation{Code: `C:\a.exe`}
	updated := Content{
		Name: old.Name,
		Tags: []string{"new"},
		Implementations: map[string]Implementation{
			OSLinux:   old.Implementations[OSLinux],
			OSWindows: {Code: `C:\b.exe`},
		},
	}
	got := Changed(old, updated)
	if want := []string{"tags", "windows"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Changed = %v, want %v", got, want)
	}
	if got := Changed(old, old); len(got) != 0 {
		t.Errorf("no change reported as %v", got)
	}
	removed := validContent()
	delete(removed.Implementations, OSLinux)
	removed.Implementations[OSDarwin] = Implementation{Code: "/bin/ls"}
	if got, want := Changed(validContent(), removed), []string{"darwin", "linux"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Changed = %v, want %v", got, want)
	}
}

func TestNormalizeDefaults(t *testing.T) {
	c := Content{
		Name: "  Disk  ",
		Tags: []string{"b", "a", "b", " ", " a "},
		Implementations: map[string]Implementation{
			OSLinux: {Code: "/bin/df", Interpreter: "shOrShebang"},
		},
	}
	Normalize(&c)
	if c.Name != "Disk" {
		t.Errorf("name %q", c.Name)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(c.Tags, want) {
		t.Errorf("tags %v, want %v", c.Tags, want)
	}
	impl := c.Implementations[OSLinux]
	if impl.TimeoutSec != 60 || impl.Kind != KindAuto || impl.Interpreter != "" {
		t.Errorf("impl not normalised: %+v", impl)
	}
	explicit := Content{Implementations: map[string]Implementation{OSLinux: {Kind: KindScript, TimeoutSec: 5}}}
	Normalize(&explicit)
	if i := explicit.Implementations[OSLinux]; i.Kind != KindScript || i.TimeoutSec != 5 {
		t.Errorf("explicit values changed: %+v", i)
	}
}

func TestValidateRejectsUnknownKind(t *testing.T) {
	c := validContent()
	c.Implementations[OSLinux] = Implementation{Kind: "foo", Code: "/bin/ls"}
	apiErr := apiError400(t, Validate(c))
	if !strings.Contains(apiErr.Message, OSLinux) || !strings.Contains(apiErr.Message, "foo") {
		t.Errorf("message %q should name the OS and the kind", apiErr.Message)
	}
}
