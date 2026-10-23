//go:build !windows

package protocol

import "os"

// ReplaceFile renames a file. The caller serializes mutations and separately
// syncs the containing directory. Windows retries brief sharing conflicts.
func ReplaceFile(from, to string) error { return os.Rename(from, to) }
