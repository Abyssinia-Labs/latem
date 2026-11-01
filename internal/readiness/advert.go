package readiness

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// What a heartbeat carries, the control plane's bounds in lib/protocol2.ts. It
// refuses the whole of a readiness over one more.
const (
	advertKinds = 16
	advertItems = 32
	advertRepos = 64
	advertText  = 120
)

// Advert is the plan as a heartbeat says it (A70): where each kind stands and
// the names of what is in the way, so that the app can show what `latem setup`
// would. It is a copy made field by field, because most of a plan belongs to
// the machine. The commands and config edits that close an item are not here,
// which is why the far side cannot act on one. Nor are paths, which a JDK's
// home and an SDK's root are full of, or what a job's steps use: the control
// plane is never told which actions a job names (INV-5) or what a secret is
// called, so those are counted.
func Advert(p Plan) *protocol.Readiness {
	out := &protocol.Readiness{Kinds: []protocol.ReadinessKind{}, Unread: len(p.Unread), Unclassified: len(p.Unclassified)}
	for _, k := range p.Kinds {
		if len(out.Kinds) == advertKinds {
			break
		}
		out.Kinds = append(out.Kinds, advertKind(k))
	}
	return out
}

func advertKind(k Kind) protocol.ReadinessKind {
	out := protocol.ReadinessKind{Kind: k.Name, Title: k.Title, State: string(k.State), Why: wireText(k.Why),
		Runner: k.Runner, Jobs: len(k.Jobs), Elsewhere: len(k.Notes)}
	out.Repos = k.Repos
	if len(out.Repos) > advertRepos {
		out.Repos = out.Repos[:advertRepos]
	}
	actions := 0
	for _, it := range k.Items {
		if strings.HasPrefix(it.ID, itemAllowed) {
			actions++
			continue
		}
		out = withItem(out, advertItem(it))
	}
	if actions > 0 {
		out = withItem(out, protocol.ReadinessItem{Title: fmt.Sprintf("%d action%s allowed on this machine", actions, pluralS(actions))})
	}
	return out
}

// advertItem is an item in words fit to leave the machine. What has a name of
// its own says only what it is.
func advertItem(it Item) protocol.ReadinessItem {
	switch {
	case strings.HasPrefix(it.ID, itemImage):
		return protocol.ReadinessItem{Title: "its image", Soft: it.Soft}
	case it.ID == itemMovingRefs:
		return protocol.ReadinessItem{Title: it.Title, Soft: it.Soft}
	}
	return protocol.ReadinessItem{Title: wireText(it.Title), Have: wireText(it.Have), Soft: it.Soft}
}

// withItem adds it, or counts it when there is no room or it has no words
// that can be sent.
func withItem(k protocol.ReadinessKind, it protocol.ReadinessItem) protocol.ReadinessKind {
	if it.Title == "" || len(k.Items) == advertItems {
		k.More++
		return k
	}
	k.Items = append(k.Items, it)
	return k
}

// wireText is s, or "" for anything that could be a path or would not read as
// one line. It is the last check and not the first: an item's words are the
// fixed phrases in this package and needs.Gaps, and a version, which a
// workflow can spell any way it likes.
func wireText(s string) string {
	if len([]rune(s)) > advertText || strings.ContainsAny(s, `/\`) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return ""
	}
	return s
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
