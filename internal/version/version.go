// Package version holds the single shared version string for all binaries.
package version

const Version = "0.71.2"

// Header is the response header the daemon stamps with Version on every HTTP
// response, and that clients compare against their own build. It lives here,
// next to the version itself, so the CLI client can read it without depending
// on the server packages.
const Header = "X-Tariboy-Version"

func UserAgent() string { return "tariboy/" + Version }
