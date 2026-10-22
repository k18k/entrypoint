// Package version holds the version set at build time (-ldflags -X).
package version

var Version = "0.1.0-dev"

// TestFault makes a test build fail on purpose ("migration" or "health"); empty in releases.
// Only the e2e update tests set it, through -ldflags, to build broken releases.
var TestFault = ""
