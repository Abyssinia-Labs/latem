package store

// HoldToken keeps a workflow job's GitHub token for the life of this process
// (A52). It is never written: a token is an hour's credential for one job, and
// the database outlives both. An agent that restarts with the job still queued
// builds it without one, and the job's log says so.
//
// The token is dropped when the run reaches a terminal state.
func (d *DB) HoldToken(runID, token string) {
	if token == "" {
		return
	}
	d.tokens.Store(runID, token)
}

// Token returns the token [DB.HoldToken] kept for a run, or "".
func (d *DB) Token(runID string) string {
	v, ok := d.tokens.Load(runID)
	if !ok {
		return ""
	}
	tok, _ := v.(string)
	return tok
}

// HoldFetchToken keeps the token a job's fetches authenticate with (A72), on
// the same terms as [DB.HoldToken]. An agent that restarts with the job still
// queued fetches with the machine's own credential.
func (d *DB) HoldFetchToken(runID, token string) {
	if token == "" {
		return
	}
	d.fetchTokens.Store(runID, token)
}

// FetchToken returns the token [DB.HoldFetchToken] kept for a run, or "".
func (d *DB) FetchToken(runID string) string {
	v, ok := d.fetchTokens.Load(runID)
	if !ok {
		return ""
	}
	tok, _ := v.(string)
	return tok
}
