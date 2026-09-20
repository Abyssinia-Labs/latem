package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Action is a parsed action.yml (A44).
//
// Only a local action is ever parsed — `uses: ./path` — so it is read from the
// same checkout, through the same [os.Root], at the same commit as the
// workflow that names it. That is what keeps INV-5 true for it without a new
// argument: an action's metadata is repository content like any other.
type Action struct {
	Name        string                  `json:"name,omitempty"`
	Description string                  `json:"description,omitempty"`
	Author      string                  `json:"author,omitempty"`
	Inputs      map[string]ActionInput  `json:"inputs,omitempty"`
	Outputs     map[string]ActionOutput `json:"outputs,omitempty"`
	Runs        ActionRuns              `json:"runs"`
	Branding    json.RawMessage         `json:"branding,omitempty"`

	// Path is the action's directory, relative to the repository root. It is
	// what `github.action_path` means inside the action.
	Path string `json:"-"`
}

// ActionInput is one declared input.
type ActionInput struct {
	Description        string  `json:"description,omitempty"`
	Required           bool    `json:"required,omitempty"`
	Default            *string `json:"default,omitempty"`
	DeprecationMessage string  `json:"deprecationMessage,omitempty"`
}

// ActionOutput is one declared output. Value is set for a composite action,
// where it is an expression over the action's own steps.
type ActionOutput struct {
	Description string `json:"description,omitempty"`
	Value       string `json:"value,omitempty"`
}

// ActionRuns is the `runs:` block. The three kinds share it; which fields mean
// anything depends on Using.
type ActionRuns struct {
	Using string `json:"using"`

	// composite
	Steps []Step `json:"steps,omitempty"`

	// docker
	Image      string            `json:"image,omitempty"`
	Entrypoint string            `json:"entrypoint,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`

	// Modelled so that strict decoding accepts a real action.yml, and refused
	// by name in [Action.kind]: JavaScript actions and the pre/post hooks are
	// phase 5.
	Main           string `json:"main,omitempty"`
	Pre            string `json:"pre,omitempty"`
	Post           string `json:"post,omitempty"`
	PreIf          string `json:"pre-if,omitempty"`
	PostIf         string `json:"post-if,omitempty"`
	PreEntrypoint  string `json:"pre-entrypoint,omitempty"`
	PostEntrypoint string `json:"post-entrypoint,omitempty"`
}

type actionKind int

const (
	actionComposite actionKind = iota
	actionDocker
)

const dockerScheme = "docker://"

// kind classifies the action, or says by name why latem cannot run it yet.
func (a *Action) kind() (actionKind, error) {
	using := strings.ToLower(strings.TrimSpace(a.Runs.Using))
	switch {
	case using == "composite":
		return actionComposite, nil
	case using == "docker":
		if a.Runs.PreEntrypoint != "" || a.Runs.PostEntrypoint != "" {
			return 0, fmt.Errorf("%s: pre-entrypoint and post-entrypoint hooks are not implemented", a.Path)
		}
		if !strings.HasPrefix(a.Runs.Image, dockerScheme) {
			return 0, fmt.Errorf(
				"%s: image %q would have to be built. latem runs an image it can pull — docker://name — "+
					"and building one from a Dockerfile is a separate surface (A44, phase 5)", a.Path, a.Runs.Image)
		}
		return actionDocker, nil
	case strings.HasPrefix(using, "node"):
		return 0, fmt.Errorf(
			"%s: a JavaScript action (%s) runs third-party code as the agent user, which is A42 phase 5; "+
				"a composite or docker:// action is what runs today", a.Path, a.Runs.Using)
	case using == "":
		return 0, fmt.Errorf("%s: runs.using is required", a.Path)
	}
	return 0, fmt.Errorf("%s: runs.using %q is not one of composite, docker", a.Path, a.Runs.Using)
}

// ActionLoader reads a local action's metadata at the resolved commit.
type ActionLoader interface {
	Load(dir string) (*Action, error)
}

// rootLoader loads actions through the workspace's [os.Root], for the reason
// gate 11 gives for the registry: a workspace holds whatever a repository put
// there, and an action directory that is a symlink to somewhere else on the
// machine is a plain thing to commit. The kernel refuses the traversal.
type rootLoader struct{ root *os.Root }

// Load reads <dir>/action.yml, or action.yaml.
func (l rootLoader) Load(dir string) (*Action, error) {
	clean := path.Clean(strings.TrimPrefix(dir, "./"))
	if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return nil, badRegistry("uses: %s reaches outside the repository", dir)
	}
	for _, name := range []string{"action.yml", "action.yaml"} {
		file := path.Join(clean, name)
		data, err := readCapped(l.root, file, MaxWorkflowBytes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			var re *registry.Error
			if errors.As(err, &re) {
				return nil, err
			}
			return nil, &registry.Error{Code: protocol.ErrBadRegistry, Msg: "reading " + file, Err: err}
		}
		return ParseAction(data, clean)
	}
	return nil, badRegistry("uses: %s — no action.yml or action.yaml there at this commit", dir)
}

// ParseAction decodes action metadata, strictly, for the reason a workflow is
// decoded strictly: an unknown key is something the file asks for that would
// otherwise be silently not done.
func ParseAction(data []byte, dir string) (*Action, error) {
	var a Action
	if err := yaml.UnmarshalStrict(data, &a); err != nil {
		return nil, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("parsing %s", path.Join(dir, "action.yml")),
			Err:  err,
		}
	}
	a.Path = dir
	return &a, nil
}
