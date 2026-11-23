package library

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	apierrors "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/validation"
)

func badRequest(format string, args ...interface{}) error {
	return apierrors.APIError{Message: fmt.Sprintf(format, args...), HTTPStatus: http.StatusBadRequest}
}

func isKnownOS(os string) bool {
	for _, k := range AllOS {
		if k == os {
			return true
		}
	}
	return false
}

// Validate checks a (normalised) content; every failure is a 400 APIError.
func Validate(c Content) error {
	if strings.TrimSpace(c.Name) == "" {
		return badRequest("name is required")
	}
	if len(c.Implementations) == 0 {
		return badRequest("at least one implementation is required")
	}
	keys := make([]string, 0, len(c.Implementations))
	for k := range c.Implementations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, os := range keys {
		impl := c.Implementations[os]
		if !isKnownOS(os) {
			return badRequest("unknown OS %q, expected one of: %s", os, strings.Join(AllOS, ", "))
		}
		if strings.TrimSpace(impl.Code) == "" {
			return badRequest("%s: code is required", os)
		}
		if impl.TimeoutSec < 0 {
			return badRequest("%s: timeout_sec must not be negative", os)
		}
		impl.Interpreter = NormalizeInterpreter(impl.Interpreter)
		if err := validation.ValidateInterpreter(impl.Interpreter, impl.EffectiveKind() == KindScript); err != nil {
			return badRequest("%s: %v", os, err)
		}
	}
	return nil
}
