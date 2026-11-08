package selfupdate

import (
	"os"
	"path/filepath"
)

// A latem that Homebrew installed (A78).
//
// Homebrew keeps each version in a directory of its own, a keg, at
// <prefix>/Cellar/latem/<version>/bin/latem, and a link at <prefix>/opt/latem
// that `brew upgrade` moves to the new keg before it deletes the old one.
// Three things in latem resolve their own path through symlinks, which is
// right for ~/.local/bin and wrong for a keg:
//
//   - a service pointed at a keg starts a binary the next `brew upgrade`
//     deletes;
//   - `latem update` and auto_update would write a release into a directory
//     Homebrew believes holds the version it installed;
//   - an agent asked to move onto the binary at its own path would look in
//     the keg it is already running from.
//
// So each asks [Homebrew] first, and for a binary in a keg uses the opt link:
// the one path that always leads to what Homebrew installed.

// brewFormula is the formula's name, which is the directory under Cellar and
// under opt, and the binary's.
const brewFormula = "latem"

// Homebrew reports whether exe, the path of the running binary, is in a
// Homebrew keg, and if it is, the link Homebrew keeps at the installed
// version: <prefix>/opt/latem/bin/latem.
//
// exe is resolved through symlinks when it can be, so the prefix's bin link
// and the opt link both count. When it cannot be, the path is read as given:
// Linux names a deleted executable by the path it had, and a keg that
// `brew upgrade` has deleted is still the keg this process came from. A path
// shaped like a keg whose prefix has no working opt link is not one.
func Homebrew(exe string) (link string, ok bool) {
	path := filepath.Clean(exe)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	bin := filepath.Dir(path) // <prefix>/Cellar/latem/<version>/bin
	keg := filepath.Dir(bin)  // <prefix>/Cellar/latem/<version>
	rack := filepath.Dir(keg) // <prefix>/Cellar/latem
	cellar := filepath.Dir(rack)
	if filepath.Base(path) != brewFormula || filepath.Base(bin) != "bin" ||
		filepath.Base(rack) != brewFormula || filepath.Base(cellar) != "Cellar" {
		return "", false
	}
	link = filepath.Join(filepath.Dir(cellar), "opt", brewFormula, "bin", brewFormula)
	if fi, err := os.Stat(link); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return link, true
}
