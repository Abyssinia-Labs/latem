package playstore

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const keytoolList = `Keystore type: PKCS12
Keystore provider: SUN

Your keystore contains 2 entries

Alias name: other
Creation date: Jan 1, 2024
Entry type: PrivateKeyEntry
Certificate chain length: 1
Certificate[1]:
Owner: CN=Other
Issuer: CN=Other
Serial number: 1
Valid from: Mon Jan 01 00:00:00 UTC 2024 until: Tue Jan 02 00:00:00 UTC 2029


*******************************************
*******************************************


Alias name: upload
Creation date: Jan 1, 2024
Entry type: PrivateKeyEntry
Certificate chain length: 2
Certificate[1]:
Owner: CN=Upload
Issuer: CN=Upload
Serial number: 2
Valid from: Mon Jan 01 00:00:00 UTC 2024 until: Sun Mar 17 12:30:00 UTC 2052
Certificate[2]:
Owner: CN=Root
Valid from: Mon Jan 01 00:00:00 UTC 2024 until: Sat Jan 01 00:00:00 UTC 2100
`

func TestExpiryOfAlias_ReadsTheEntrysOwnCertificate(t *testing.T) {
	got, found := expiryOfAlias([]byte(keytoolList), "upload")
	want := time.Date(2052, 3, 17, 12, 30, 0, 0, time.UTC)
	if !found || !got.Equal(want) {
		t.Errorf("got %v, %v", got, found)
	}
}

func TestExpiryOfAlias_MatchesAsGradleDoes(t *testing.T) {
	if _, found := expiryOfAlias([]byte(keytoolList), "UPLOAD"); !found {
		t.Error("a JKS keystore lowercases its aliases")
	}
}

func TestExpiryOfAlias_AnotherAliasIsNotThisOne(t *testing.T) {
	if _, found := expiryOfAlias([]byte(keytoolList), "absent"); found {
		t.Error("found an alias that is not there")
	}
}

func TestExpiryOfAlias_ADateItCannotReadIsStillTheAlias(t *testing.T) {
	got, found := expiryOfAlias([]byte("Alias name: upload\nValid from: x until: not a date\n"), "upload")
	if !found || !got.IsZero() {
		t.Errorf("got %v, %v", got, found)
	}
}

// fakeKeytool writes a shell script standing in for keytool: the tests have no
// JDK, and what they care about is what it is handed. It records its argv and
// the environment's password, and its exit and output are the test's.
func fakeKeytool(t *testing.T, output string, exit string) (path, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	path = filepath.Join(dir, "keytool")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$*" > "` + logPath + `"` + "\n" +
		`printf '%s\n' "$LATEM_KEYSTORE_PASSWORD" >> "` + logPath + `"` + "\n" +
		`while [ $# -gt 0 ]; do [ "$1" = -keystore ] && ls -l "$2" >> "` + logPath + `"; shift; done` + "\n" +
		"cat <<'OUT'\n" + output + "\nOUT\n" +
		"exit " + exit + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable for this test alone
		t.Fatal(err)
	}
	return path, logPath
}

var testKeystore = []byte(base64.StdEncoding.EncodeToString([]byte("not really a keystore")))

func TestReadUploadKey_ThePasswordAndAliasStayOutOfArgv(t *testing.T) {
	keytool, logPath := fakeKeytool(t, keytoolList, "0")
	key, err := ReadUploadKey(t.Context(), keytool, testKeystore, []byte("hunter2-password"), "upload")
	if err != nil {
		t.Fatal(err)
	}
	if key.Expires.Year() != 2052 {
		t.Errorf("expires %v", key.Expires)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if argv := lines[0]; strings.Contains(argv, "hunter2-password") || strings.Contains(argv, "upload ") {
		t.Errorf("a secret reached argv: %q", argv)
	}
	if !strings.Contains(lines[0], "-storepass:env LATEM_KEYSTORE_PASSWORD") {
		t.Errorf("argv: %q", lines[0])
	}
	if lines[1] != "hunter2-password" {
		t.Errorf("the password did not arrive in the environment: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "-rw-------") {
		t.Errorf("the keystore on disk should be for this user alone: %q", lines[2])
	}
}

func TestReadUploadKey_LeavesNothingBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, exit := range []string{"0", "1"} {
		keytool, _ := fakeKeytool(t, keytoolList, exit)
		_, _ = ReadUploadKey(t.Context(), keytool, testKeystore, []byte("p"), "upload")
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("left %d entries in the temp directory", len(left))
	}
}

func TestReadUploadKey_NamesItsFailuresAndQuotesNoOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		output, exit string
		want         error
	}{
		"pkcs12 password": {"keytool error: java.io.IOException: keystore password was incorrect", "1", ErrKeystorePassword},
		"jks password":    {"keytool error: java.io.IOException: Keystore was tampered with, or password was incorrect", "1", ErrKeystorePassword},
		"no alias":        {keytoolList, "0", ErrNoAlias},
		"stub":            {"The operation couldn't be completed. Unable to locate a Java Runtime.", "1", ErrNoKeytool},
	} {
		t.Run(name, func(t *testing.T) {
			keytool, _ := fakeKeytool(t, tc.output, tc.exit)
			alias := "upload"
			if name == "no alias" {
				alias = "absent"
			}
			_, err := ReadUploadKey(t.Context(), keytool, testKeystore, []byte("p"), alias)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}

	keytool, _ := fakeKeytool(t, "keytool error: alias-sentinel in the output", "1")
	_, err := ReadUploadKey(t.Context(), keytool, testKeystore, []byte("p"), "upload")
	if err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("keytool's output holds the alias and must not reach an error: %v", err)
	}
}

func TestReadUploadKey_RefusesWhatIsNotBase64(t *testing.T) {
	keytool, _ := fakeKeytool(t, "", "0")
	_, err := ReadUploadKey(t.Context(), keytool, []byte("!!! sentinel !!!"), []byte("p"), "upload")
	if err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("got %v", err)
	}
}

func TestReadUploadKey_ToleratesTheNewlinesBase64Wraps(t *testing.T) {
	keytool, _ := fakeKeytool(t, keytoolList, "0")
	long := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("keystore", 40)))
	wrapped := long[:64] + "\n" + long[64:] + "\n"
	if _, err := ReadUploadKey(t.Context(), keytool, []byte(wrapped), []byte("p"), "upload"); err != nil {
		t.Fatal(err)
	}
}

func TestReadUploadKey_NoKeytoolSaysSo(t *testing.T) {
	if _, err := ReadUploadKey(t.Context(), "", testKeystore, []byte("p"), "upload"); !errors.Is(err, ErrNoKeytool) {
		t.Fatalf("got %v", err)
	}
}

func TestFindKeytool_OrderIsJavaHomeThenKegsThenPath(t *testing.T) {
	root := t.TempDir()
	tool := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable for this test alone
			t.Fatal(err)
		}
		return p
	}
	onPath := func(string) (string, error) { return "/usr/bin/keytool", nil }
	nowhere := func(string) (string, error) { return "", errors.New("not found") }

	home := tool("home", "bin", "keytool")
	old := tool("opt", "openjdk@17", "bin", "keytool")
	newest := tool("opt", "openjdk@21", "bin", "keytool")
	kegs := filepath.Join(root, "opt")

	if got := FindKeytool(filepath.Dir(filepath.Dir(home)), kegs, onPath); got != home {
		t.Errorf("JAVA_HOME first: %q", got)
	}
	if got := FindKeytool("", kegs, onPath); got != newest {
		t.Errorf("the newest keg before the PATH (a Mac's is a stub): %q, and %q exists", got, old)
	}
	if got := FindKeytool("", "", onPath); got != "/usr/bin/keytool" {
		t.Errorf("the PATH last: %q", got)
	}
	if got := FindKeytool("", "", nowhere); got != "" {
		t.Errorf("nothing at all: %q", got)
	}
}
