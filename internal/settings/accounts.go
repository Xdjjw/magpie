package settings

import (
	"os"
	"strings"
)

// AccountDiscoveryOff is whether magpie leaves agents' sign-ins unread
// (#1535): NoAccountDiscovery saved, or MAGPIE_NO_ACCOUNTS set to anything
// but "", "0" or "false" for this process alone. The variable only ever
// turns discovery off; it never turns on what the user saved off.
func AccountDiscoveryOff() bool {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_NO_ACCOUNTS"))); v != "" && v != "0" && v != "false" {
		return true
	}
	return Load().NoAccountDiscovery
}
