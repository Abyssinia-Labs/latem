package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Variables is what OpenVault holds of one repository's workflow variables
// (A96): by environment, "" for the ones at the top of the collection, and
// then by name.
type Variables map[string]map[string]string

// variableName is GitHub's rule for a variable's name, which config.toml's
// variables answer to as well. A vault entry by any other name is no
// variable a workflow could read.
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Variables reads collection's variables (A96): every entry of kind variable
// at its top, NAME, and one folder deep, E/NAME, for each environment E that
// envs admits. A nil envs admits every one.
//
// Only an entry of kind variable answers. A secret is masked in a job's log
// and a variable is not, so a secret must never reach the vars context, even
// one with a variable's name. An entry the listing calls a variable and the
// read finds is no longer one, because it changed in between, is left out for
// the same reason. A file is a path, never a value.
//
// It is one listing, from the index, and one read of each variable it admits.
// A collection this machine does not hold is [ErrNoCollection]: the vault has
// none of the repository's variables here, and config.toml's answer. A service
// that does not answer is an [*OpenVaultError], as for a secret.
func (p *OpenVaultProvider) Variables(ctx context.Context, collection string, envs func(env string) bool) (Variables, error) {
	if !openVaultCollection.MatchString(collection) {
		return nil, fmt.Errorf("openvault: repository %q cannot be a collection's name: %w", collection, ErrNotFound)
	}
	entries, err := p.listing(ctx, collection)
	if err != nil {
		var ove *OpenVaultError
		if errors.As(err, &ove) && ove.Refused {
			if names, cerr := p.Collections(ctx); cerr == nil && !slices.Contains(names, collection) {
				return nil, fmt.Errorf("openvault: %s: %w", collection, ErrNoCollection)
			}
		}
		return nil, err
	}
	out := Variables{}
	for _, e := range entries {
		if e.kind != ovVar {
			continue
		}
		env, name, nested := strings.Cut(e.name, "/")
		if !nested {
			env, name = "", e.name
		}
		if strings.Contains(name, "/") || !variableName.MatchString(name) ||
			strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
			continue
		}
		if _, _, err := OpenVaultEntry(scopeOf(collection, env), name); err != nil {
			continue
		}
		if envs != nil && !envs(env) {
			continue
		}
		value, ok, err := p.variable(ctx, collection, e.name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if out[env] == nil {
			out[env] = map[string]string{}
		}
		out[env][name] = value
	}
	return out, nil
}

func scopeOf(collection, env string) []string {
	if env == "" {
		return []string{collection}
	}
	return []string{collection, env}
}

// variable is one entry's value, if it is still a variable.
func (p *OpenVaultProvider) variable(ctx context.Context, collection, entry string) (string, bool, error) {
	typ, body, err := p.request(ctx, ovGet, ovStr(collection), ovStr(entry))
	if err != nil {
		return "", false, err
	}
	defer zero(body)
	switch typ {
	case ovEntry:
		r := ovReader{b: body}
		switch found := r.u8(); {
		case r.err == nil && found == 0:
			// Removed since the listing: the vault no longer holds it.
			return "", false, nil
		case found != 1:
			return "", false, p.protocolError("get", "an entry that does not parse")
		}
		kind := r.u8()
		value := r.bytes()
		if r.err != nil || !r.done() {
			return "", false, p.protocolError("get", "an entry that does not parse")
		}
		if kind != ovVar {
			return "", false, nil
		}
		return string(value), true, nil
	case ovError:
		return "", false, p.serviceError("get", body)
	}
	return "", false, p.protocolError("get", fmt.Sprintf("message type 0x%02x", typ))
}
