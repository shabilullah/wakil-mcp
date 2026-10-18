// Package version carries build identity, set at link time.
package version

// These are overridden with -ldflags at release time.
var (
	// Version is the semantic version of the build.
	Version = "dev"
	// Commit is the git revision the build came from.
	Commit = "none"
	// Date is the build timestamp.
	Date = "unknown"
)

// String renders a single-line build identifier.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
