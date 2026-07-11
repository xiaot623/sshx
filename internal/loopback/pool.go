// Package loopback defines the target-specific loopback address pool shared by
// the local daemon and platform setup code.
package loopback

import "strconv"

const (
	// Prefix is kept away from 127.0.0.1, which is used by the local DNS
	// responder and by unrelated localhost services.
	Prefix = "127.64.0."

	// Size is the number of target addresses provisioned on platforms that do
	// not treat the entire 127.0.0.0/8 block as loopback (notably macOS).
	Size = 64
)

// Address returns the zero-based address at index, or an empty string when the
// index is outside the provisioned pool.
func Address(index int) string {
	if index < 0 || index >= Size {
		return ""
	}
	return Prefix + strconv.Itoa(index+1)
}
