package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// DefaultVarsInterval is how often the agent reads OpenVault's variables
// again for the heartbeat's digests (A96). A run reads them afresh as it
// starts, so this decides only how long another machine can see this one's
// old answer after a value changes in the vault: about a minute, against a
// listing and a read per variable, each a local socket call, where reading
// on every beat would be four times that for an answer nobody needs sooner.
const DefaultVarsInterval = time.Minute

// varsTimeout bounds one look at the vault's variables, so a service that
// accepts and never answers holds the next look up by no more than this.
const varsTimeout = 10 * time.Second

// ownVars admits a repository's own variables, outside any environment: the
// only ones a heartbeat carries (A68).
func ownVars(env string) bool { return env == "" }

// vaultDigests is what the heartbeat says of the variables OpenVault holds,
// as last read (A96): for each repository, the digest of each of its own.
//
// It is digests, not values, so that it can be kept on disk across a restart:
// a heartbeat carries nothing more, and a machine that starts before its
// OpenVault service answers then says what it said before it stopped, rather
// than config.toml's answer alone, which would set it apart from the machines
// that share the vault until the service came up.
type vaultDigests struct {
	mu sync.Mutex
	// repos holds a repository once its variables are known: read since the
	// agent started, or kept from before. An empty map is a repository the
	// vault holds no variable of.
	repos map[string]map[string]string
	// down is set while the service does not answer, so that it is logged
	// once and not every minute.
	down bool
}

func (v *vaultDigests) of(repo string) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.repos[repo]
}

// varDigestsFile is [config.Paths.VarDigests] as written.
type varDigestsFile struct {
	Repos map[string]map[string]string `json:"repos"`
}

// varsLoop keeps the vault's part of the heartbeat's digests current (A96),
// on a machine whose config reads OpenVault. It reads once at start, then
// every varsEvery and on a reload, and writes the file only when an answer
// changed.
func (a *Agent) varsLoop(ctx context.Context) {
	if a.vault == nil {
		return
	}
	t := time.NewTicker(a.varsEvery)
	defer t.Stop()
	for {
		a.refreshVars(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.rereadVars:
		}
	}
}

// refreshVars reads every configured repository's own variables from the
// vault. A service that does not answer changes nothing: the heartbeat goes
// on saying what was read last, because a job that reads a variable fails
// retryably meanwhile and the answer once the service is back is the vault's,
// and the machine should not look different from its peers over a blip.
func (a *Agent) refreshVars(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, varsTimeout)
	defer cancel()
	read := map[string]map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(a.current().Repos)) {
		v, err := a.vault.Variables(ctx, key, ownVars)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			read[key] = map[string]string{}
		case err != nil:
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return
			}
			a.vaultVars.mu.Lock()
			first := !a.vaultVars.down
			a.vaultVars.down = true
			a.vaultVars.mu.Unlock()
			if first {
				a.log.Warn("the vault did not answer for the repositories' variables; the heartbeat keeps "+
					"the digests it read last, and a job that reads a variable fails until it answers", "err", err)
			}
			return
		default:
			read[key] = varDigests(v[""])
		}
	}

	a.vaultVars.mu.Lock()
	recovered := a.vaultVars.down
	a.vaultVars.down = false
	changed := !maps.EqualFunc(a.vaultVars.repos, read, maps.Equal[map[string]string])
	var names []string
	if changed {
		for key, d := range read {
			if !maps.Equal(a.vaultVars.repos[key], d) {
				names = append(names, key)
			}
		}
		a.vaultVars.repos = read
	}
	a.vaultVars.mu.Unlock()

	if recovered {
		a.log.Info("the vault answers again for the repositories' variables")
	}
	if !changed {
		return
	}
	slices.Sort(names)
	a.log.Info("variables in the vault read", "repos", names)
	if err := writeVarDigests(a.cfg.Paths.VarDigests, read); err != nil {
		a.log.Warn("could not keep the variables' digests for the next start", "err", err)
	} else if old := a.cfg.Paths.OldVarDigests; old != "" {
		// The new file answers from now on; the old one would only go stale.
		_ = os.Remove(old)
	}
}

// loadVarDigests takes what the last agent read of the vault's variables,
// until this one reads them itself. A file that is missing or does not parse
// is no answer, and config.toml's stands meanwhile.
func (a *Agent) loadVarDigests() {
	if a.vault == nil || a.cfg.Paths.VarDigests == "" {
		return
	}
	b, err := os.ReadFile(a.cfg.Paths.VarDigests)
	if errors.Is(err, os.ErrNotExist) && a.cfg.Paths.OldVarDigests != "" {
		// The file's name before latem vault was renamed (A104), until this
		// agent writes the new one.
		b, err = os.ReadFile(a.cfg.Paths.OldVarDigests)
	}
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.log.Warn("could not read the variables' digests kept from the last start", "err", err)
		}
		return
	}
	var f varDigestsFile
	if err := json.Unmarshal(b, &f); err != nil {
		a.log.Warn("the variables' digests kept from the last start do not parse; config.toml's answer until "+
			"the vault does", "err", err)
		return
	}
	a.vaultVars.mu.Lock()
	defer a.vaultVars.mu.Unlock()
	if a.vaultVars.repos == nil {
		a.vaultVars.repos = f.Repos
	}
}

// writeVarDigests replaces the file whole, so that a crash mid-write leaves
// the last one.
func writeVarDigests(path string, repos map[string]map[string]string) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(varDigestsFile{Repos: repos})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vault-vars-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// heartbeatVars is what a heartbeat says of a repository's variables (A68,
// A96): config.toml's own, and the vault's over them as last read, which is
// the order [repovars.Set.For] reads them in for a job.
func (a *Agent) heartbeatVars(key string, r *config.Repo) map[string]string {
	out := varDigests(r.VarsFor(""))
	if a.vault == nil {
		return out
	}
	maps.Copy(out, a.vaultVars.of(key))
	return out
}

// rereadVarsSoon asks varsLoop to read the vault now: a reload may have
// added a repository.
func (a *Agent) rereadVarsSoon() {
	select {
	case a.rereadVars <- struct{}{}:
	default:
	}
}
