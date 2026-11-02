package config

import (
	"net/url"
	"regexp"
	"strings"
)

// githubSlugPattern is owner/name as GitHub allows them: an owner of letters,
// digits and hyphens, a name that may also hold dots and underscores.
var githubSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// GitHubRepository is the repository's owner/name (A50): [Repo.GitHub] when it
// is set, otherwise read from a URL that names github.com. The bool is false
// when neither says, which is the case for a local path; the caller then falls
// back to the config key, as every run did before A50.
func (r Repo) GitHubRepository() (string, bool) {
	if r.GitHub != "" {
		return r.GitHub, true
	}
	return githubSlugFromURL(r.URL)
}

// FetchToken is what a fetch of r may send of the token its work came with
// (A72). An installation token is GitHub's, so a URL anywhere else gets none
// and keeps the machine's own credential, whatever `github` says it stands for.
func (r Repo) FetchToken(token string) string {
	if _, ok := githubSlugFromURL(r.URL); !ok {
		return ""
	}
	return token
}

// githubSlugFromURL reads owner/name from the URL forms git accepts for
// github.com: https://github.com/o/n(.git), ssh://git@github.com/o/n(.git) and
// the scp-like git@github.com:o/n(.git).
func githubSlugFromURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	var rest string
	if after, ok := strings.CutPrefix(s, "git@github.com:"); ok {
		rest = after
	} else {
		u, err := url.Parse(s)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", false
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return "", false
		}
		rest = strings.TrimPrefix(u.Path, "/")
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	if !githubSlugPattern.MatchString(rest) {
		return "", false
	}
	return rest, true
}
