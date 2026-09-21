package main

import (
	"strings"
)

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}

	return b
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}

	return b
}

// isTSFile reports whether name looks like a transport-stream
// recording. DTV STBs ship either `.ts` or `.tsv` depending on
// vendor; some also use `.m2ts` / `.trp`, so keep this easy to
// extend.
func isTSFile(name string) bool {
	lower := strings.ToLower(name)

	switch {
	case strings.HasSuffix(lower, ".ts"):
		return true
	case strings.HasSuffix(lower, ".tsv"):
		return true
	case strings.HasSuffix(lower, ".m2ts"):
		return true
	case strings.HasSuffix(lower, ".trp"):
		return true
	}

	return false
}
