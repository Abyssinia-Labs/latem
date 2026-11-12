package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// File is a secret that is a file (A88): an OpenVault file entry, read whole.
// A step gets it as the path of a private copy in the job's temporary
// directory, under Name, rather than as its bytes, which an environment
// variable cannot carry when they are a keystore's.
type File struct {
	// Name is the file's own name, the last part of its entry's name, which
	// the copy keeps: `upload.jks` for `release/upload.jks`.
	Name string
	// Data is the file's bytes. Zeroed with the [Set] that holds it.
	Data Value
}

// FileScopedProvider is a [ScopedProvider] that also holds files (A88): asked
// for name under scope, it answers with the file whose [FileVariable] is
// name. Only OpenVault does.
type FileScopedProvider interface {
	ScopedProvider
	// ResolveFileIn returns the file under scope that sets name, or an
	// [ErrNotFound] when there is none.
	ResolveFileIn(ctx context.Context, scope []string, name string) (File, error)
}

// FileVariable is the name a file's path is read as, OpenVault's rule for
// `openvault run` (files.md, run): the last part of its name, upper-cased,
// with every character but A–Z and 0–9 turned into `_`. `release/upload.jks`
// is UPLOAD_JKS. It is not a usable name when it starts with a digit, which
// [ValidName] tells.
func FileVariable(entry string) string {
	last := entry[strings.LastIndexByte(entry, '/')+1:]
	var b strings.Builder
	for _, r := range strings.ToUpper(last) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ResolveFileIn asks the chain's file providers, in order, for the file under
// scope that sets name. First found wins; a provider that cannot answer
// stops the chain, as [Chain.ResolveInFrom] does.
func (c *Chain) ResolveFileIn(ctx context.Context, scope []string, name string) (File, string, error) {
	var misses error
	for _, p := range c.providers {
		fp, ok := p.(FileScopedProvider)
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return File{}, "", err
		}
		f, err := fp.ResolveFileIn(ctx, scope, name)
		switch {
		case err == nil:
			source := p.Name()
			if l, ok := p.(fileLocator); ok {
				if where := l.WhereFile(scope, f); where != "" {
					source += " as " + where
				}
			}
			return f, source, nil
		case errors.Is(err, ErrNotFound):
			misses = errors.Join(misses, err)
		default:
			return File{}, "", err
		}
	}
	if misses == nil {
		misses = fmt.Errorf("secrets: no store holds files: %w", ErrNotFound)
	}
	return File{}, "", misses
}

// fileLocator says where a provider found a file, for the run's log.
type fileLocator interface {
	WhereFile(scope []string, f File) string
}

// resolveFileLookup asks p for the file l names: a [Chain] asks its file
// providers, a [FileScopedProvider] answers itself, and any other provider
// holds no files.
func resolveFileLookup(ctx context.Context, p Provider, l Lookup) (File, string, error) {
	switch s := p.(type) {
	case *Chain:
		return s.ResolveFileIn(ctx, l.Scope, l.Name)
	case FileScopedProvider:
		f, err := s.ResolveFileIn(ctx, l.Scope, l.Name)
		return f, p.Name(), err
	}
	return File{}, "", fmt.Errorf("%s holds no files: %w", p.Name(), ErrNotFound)
}

// redactableFile is whether a file's bytes are text the redactor can match: a
// `.p8` or a service account's JSON is, a keystore is not, and binary bytes
// cut into lines would only make noise in the log.
func redactableFile(data []byte) bool {
	return utf8.Valid(data)
}
