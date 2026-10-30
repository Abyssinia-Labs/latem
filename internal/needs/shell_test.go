package needs

import (
	"reflect"
	"strings"
	"testing"
)

func firstWords(script string) []string {
	var out []string
	for _, c := range commands(script) {
		out = append(out, strings.Join(c.words, " "))
	}
	return out
}

func TestCommands(t *testing.T) {
	for name, c := range map[string]struct {
		script string
		want   []string
	}{
		"plain": {"bun install\nbun test", []string{"bun install", "bun test"}},
		"separators": {"make && make test || echo no; go vet ./... | tee out",
			[]string{"make", "make test", "go vet ./...", "tee out"}},
		"a comment is not a command": {"# xcodebuild -scheme App\nbun test # codesign later",
			[]string{"bun test"}},
		"a hash inside a word is not a comment": {"curl https://x/y#frag", []string{"curl https://x/y#frag"}},
		"quoted words are arguments": {`echo "xcodebuild is not run here"; gh release create "v1"`,
			[]string{"gh release create v1"}},
		"a substitution in a string is a command": {`DEVELOPER_DIR="$(scripts/find-xcode.sh 27)" xcodebuild -version`,
			[]string{"xcodebuild -version", "scripts/find-xcode.sh 27"}},
		"backticks":                   {"v=`xcrun --show-sdk-version`", []string{"xcrun --show-sdk-version"}},
		"arithmetic is not a command": {"n=$((1 + 2))\nsleep $n", []string{"sleep $n"}},
		"a heredoc body is data": {"cat <<'EOF' > notes.md\nrun xcodebuild and gradlew\nEOF\ngo test",
			[]string{"cat", "go test"}},
		"a dash heredoc": {"cat <<-END\n\tdocker build .\n\tEND\nbun test", []string{"cat", "bun test"}},
		"continuation":   {"xcodebuild \\\n  -scheme App \\\n  build", []string{"xcodebuild -scheme App build"}},
		"wrappers": {"sudo -E apt-get install -y minisign\nenv FOO=1 bundle exec fastlane beta\ntimeout 30 bunx wrangler deploy",
			[]string{"apt-get install -y minisign", "fastlane beta", "wrangler deploy"}},
		"lookups run nothing": {"command -v go || brew install go\nwhich xcodebuild", []string{"brew install go"}},
		"keywords":            {"if ! docker info; then exit 1; fi", []string{"docker info"}},
		"redirections":        {"go test ./... 2>&1 > log.txt", []string{"go test ./..."}},
		"a string across lines": {"node -e \"\nconsole.log('xcodebuild')\n\"\nbun test",
			[]string{"node -e \nconsole.log('xcodebuild')\n", "bun test"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstWords(c.script); !reflect.DeepEqual(got, c.want) {
				t.Errorf("commands(%q)\n got %q\nwant %q", c.script, got, c.want)
			}
		})
	}
}

// What a line set before its command, or exported earlier, is the command's.
func TestCommands_Environment(t *testing.T) {
	cs := commands("export DEVELOPER_DIR=/Applications/Xcode_27.app\nDEBUG=1 xcodebuild -version")
	if len(cs) != 1 || cs[0].env["DEVELOPER_DIR"] != "/Applications/Xcode_27.app" || cs[0].env["DEBUG"] != "1" {
		t.Errorf("commands = %+v", cs)
	}
	if cs[0].line != 2 || cs[0].text != "DEBUG=1 xcodebuild -version" {
		t.Errorf("line %d %q", cs[0].line, cs[0].text)
	}
}

func FuzzCommands(f *testing.F) {
	for _, s := range []string{"a && b", "x=\"$(y $(z))\"", "cat <<E\nq\nE", "'\"", "\\", "$((", "`", "a <<- 'X'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, c := range commands(s) {
			if len(c.words) == 0 {
				t.Fatalf("a command with no words from %q", s)
			}
		}
	})
}
