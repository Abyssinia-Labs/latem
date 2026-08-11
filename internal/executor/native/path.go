package native

import (
	"path/filepath"
	"strings"
)

// within reports whether p is root or is inside it.
//
// This is all that survives of this package's artifact globber, which was
// removed with [executor.Env.Fetch] in amendment A29. It is kept because cache
// linking needs it for a different reason: §14.2 shares a cache into the
// workspace by symlink, and a link whose target has been repointed outside the
// cache root must be refused rather than followed.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
