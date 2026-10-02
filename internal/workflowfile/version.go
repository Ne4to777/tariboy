package workflowfile

import "github.com/alekzonder/tariboy/internal/imagefile"

const versionKey = "workflow_version"

// GetVersion reads workflow_version from a Workflowfile.yaml or its directory.
func GetVersion(path string) (string, error) {
	return imagefile.GetVersionField(path, versionKey, DefaultFilename)
}

// UpdateVersion bumps major, minor, or patch, resets lower parts, drops
// suffixes, and rewrites the file atomically, preserving permissions.
func UpdateVersion(path, part string) (string, error) {
	return imagefile.UpdateVersionField(path, versionKey, DefaultFilename, part)
}
