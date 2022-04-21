/*
Package build contains build specific information.
*/
package build

// gitRevision is injected at build time.
var gitRevision string

// GetGitRevision retrieves the revision injected into the build at build time.
func GetGitRevision() string {
	return gitRevision
}
