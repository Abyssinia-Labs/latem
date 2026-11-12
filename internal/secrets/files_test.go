package secrets

import (
	"context"
	"slices"
	"testing"
)

// fileVault holds values and files by name, for any scope.
type fileVault struct {
	values map[string]string
	files  map[string]File
}

func (fileVault) Name() string { return "vault" }

func (v fileVault) Resolve(ctx context.Context, name string) (Value, error) {
	return v.ResolveIn(ctx, nil, name)
}

func (v fileVault) ResolveIn(_ context.Context, _ []string, name string) (Value, error) {
	if value, ok := v.values[name]; ok {
		return Value(value), nil
	}
	return nil, ErrNotFound
}

func (v fileVault) ResolveFileIn(_ context.Context, _ []string, name string) (File, error) {
	if f, ok := v.files[name]; ok {
		return File{Name: f.Name, Data: append(Value(nil), f.Data...)}, nil
	}
	return File{}, ErrNotFound
}

func TestFileVariableIsOpenVaultsRule(t *testing.T) {
	for entry, want := range map[string]string{
		"upload.jks":                       "UPLOAD_JKS",
		"production/google-services.json":  "GOOGLE_SERVICES_JSON",
		"production/ios/AuthKey_ABC123.p8": "AUTHKEY_ABC123_P8",
		"release/2fa.txt":                  "2FA_TXT",
	} {
		if got := FileVariable(entry); got != want {
			t.Errorf("FileVariable(%q) = %q, want %q", entry, got, want)
		}
	}
	if ValidName(FileVariable("2fa.txt")) == nil {
		t.Error("a name starting with a digit was usable")
	}
}

// A name no secret or variable answers to resolves to the file that sets it
// (A88), which the set remembers is a file; a secret by the name comes first.
// Only a text file's bytes are registered for redaction.
func TestResolveAs_ANameThatIsAFileResolvesToItsBytes(t *testing.T) {
	keystore := "\x00\xfekeystore\x00bytes"
	vault := fileVault{
		values: map[string]string{"TOKEN": "token-value-123", "BOTH": "the-secret-wins"},
		files: map[string]File{
			"UPLOAD_JKS":    {Name: "upload.jks", Data: Value(keystore)},
			"PLAY_KEY_JSON": {Name: "play-key.json", Data: Value(`{"private_key":"pk-1234567890"}`)},
			"BOTH":          {Name: "both.txt", Data: Value("the-file-loses")},
		},
	}
	reg := &fakeRegistrar{}
	set, missing, err := ResolveAs(context.Background(), NewChain(vault), reg,
		[]string{"TOKEN", "UPLOAD_JKS", "PLAY_KEY_JSON", "BOTH", "NOWHERE"},
		func(name string) []Lookup { return []Lookup{{Scope: []string{"app"}, Name: name}} })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"NOWHERE"}) {
		t.Errorf("missing = %v", missing)
	}
	for name, want := range map[string]struct {
		file, data string
	}{
		"UPLOAD_JKS":    {"upload.jks", keystore},
		"PLAY_KEY_JSON": {"play-key.json", `{"private_key":"pk-1234567890"}`},
		"BOTH":          {"", "the-secret-wins"},
		"TOKEN":         {"", "token-value-123"},
	} {
		file, isFile := set.FileName(name)
		value, _ := set.Value(name)
		if file != want.file || isFile != (want.file != "") || string(value) != want.data {
			t.Errorf("%s = file %q (%v), %q; want %q, %q", name, file, isFile, value, want.file, want.data)
		}
	}
	registered := reg.registered()
	if slices.Contains(registered, keystore) {
		t.Error("a keystore's bytes were registered for redaction")
	}
	if !slices.Contains(registered, `{"private_key":"pk-1234567890"}`) {
		t.Errorf("a text file wasn't registered: %q", registered)
	}
	set.Zero()
	if _, ok := set.Value("UPLOAD_JKS"); ok {
		t.Error("a file's bytes survived Zero")
	}
}
