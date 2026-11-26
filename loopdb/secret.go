package loopdb

import (
	"fmt"
	"os"
	"strings"
)

// Secret is a string type that can unmarshal values from files when prefixed
// with '@'. This allows sensitive values like passwords to be stored in files
// rather than directly in configuration.
type Secret string

// UnmarshalFlag implements go-flags Unmarshaler. If value starts with '@',
// reads from file at that path. A value starting with '@@' is used verbatim
// with the leading '@' removed, which is how a secret that itself begins with
// '@' is expressed. Otherwise uses value directly.
func (s *Secret) UnmarshalFlag(value string) error {
	// A doubled '@' escapes a secret beginning with a literal '@'. Drop
	// the escape character and use the remainder as-is.
	if strings.HasPrefix(value, "@@") {
		*s = Secret(value[1:])

		return nil
	}

	if strings.HasPrefix(value, "@") {
		filePath := value[1:]
		content, err := os.ReadFile(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("secret file not found: %s",
					filePath)
			}
			if os.IsPermission(err) {
				return fmt.Errorf("unable to read secret "+
					"file (permission denied): %s",
					filePath)
			}

			return fmt.Errorf("failed to read secret file %s: %w",
				filePath, err)
		}
		// Trim trailing whitespace (spaces, tabs, newlines) to handle
		// files created on Windows (CRLF) or Unix (LF), and to avoid
		// invisible trailing spaces causing authentication failures.
		*s = Secret(strings.TrimRight(string(content), " \t\r\n"))

		return nil
	}
	*s = Secret(value)

	return nil
}
