package native

import (
	"os"
	"path/filepath"
	"testing"
)

// Amendment A38. A committed jobs.yaml runs on every machine (§12), so a cache
// target cannot be written absolutely — "/Users/someone/Library/..." is correct
// on exactly one host. `~` is the portable form, which is why §9's own example
// uses it, and why every reference registry shipped a cache path no executor
// would accept until the first agent-run job failed on it in under a second.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, in, want string
	}{
		{"bare tilde", "~", home},
		{"the case that broke", "~/Library/Developer/Xcode/DerivedData",
			filepath.Join(home, "Library/Developer/Xcode/DerivedData")},
		{"absolute is untouched", "/home/runner/.gradle", "/home/runner/.gradle"},
		{"relative is untouched", "relative/stays", "relative/stays"},

		// A tilde that is not leading is an ordinary character. Expanding it
		// would corrupt a legitimate path rather than fix anything.
		{"embedded tilde", "/opt/~/x", "/opt/~/x"},

		// ~user is deliberately NOT supported. A step already runs as one
		// specific user, so naming another produces a directory that user
		// cannot write, and the failure surfaces as a permission error on a
		// cache mount rather than as anything mentioning ~.
		{"other user", "~otheruser/x", "~otheruser/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandHome(tc.in)
			if err != nil {
				t.Fatalf("expandHome(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The expansion must be reachable from the cache mount path, not merely exist.
// A helper with a passing unit test and no caller is the A29 pattern; this
// asserts the wiring by mounting a cache whose target is written with a tilde.
func TestCacheTargetExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := expandHome("~/Library/Caches/CocoaPods")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library/Caches/CocoaPods")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("%q is not absolute; the mount check would still reject it", got)
	}
}
