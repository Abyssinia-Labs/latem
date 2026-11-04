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
// plane is never told which actions a job names (INV-5), so those are
// counted. A secret's name is said, never its value (A73): it is what a
// person has to know to set it here or copy it from another machine.
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
		out = withItem(out, advertItem(it, len(k.Jobs)))
	}
	if actions > 0 {
		out = withItem(out, protocol.ReadinessItem{Title: fmt.Sprintf("%d action%s allowed on this machine", actions, pluralS(actions))})
	}
	return out
}

// advertItem is an item in words fit to leave the machine. What has a name of
// its own says only what it is. An item that holds up some of a kind's jobs and
// not the rest says how many, because the kind is flagged over one.
func advertItem(it Item, jobs int) protocol.ReadinessItem {
	out := protocol.ReadinessItem{Title: wireText(it.Title), Have: wireText(it.Have), Soft: it.Soft}
	switch {
	case strings.HasPrefix(it.ID, itemImage):
		out.Title, out.Have = "its image", ""
	case it.ID == itemMovingRefs:
		out.Have = ""
	}
	count := ""
	if n := len(it.Jobs); n > 0 && n < jobs {
		count = fmt.Sprintf("%d of %d jobs", n, jobs)
	}
	if strings.HasPrefix(it.ID, itemSecrets) && len(it.Secrets) > 0 {
		budget := advertText
		if count != "" {
			budget -= len(", ") + len(count)
		}
		if names := secretNames(it.Secrets, budget); names != "" {
			out.Have = names
		}
	}
	if count != "" {
		if out.Have != "" {
			count = out.Have + ", " + count
		}
		out.Have = wireText(count)
	}
	return out
}

// secretNames is "A, B (staging) not set" in at most budget characters,
// ending "and N more" when they do not all fit, or "" when not even one does
// or a name is not fit to send.
func secretNames(names []string, budget int) string {
	for _, n := range names {
		if wireText(n) == "" {
			return ""
		}
	}
	for shown := len(names); shown > 0; shown-- {
		s := strings.Join(names[:shown], ", ")
		if rest := len(names) - shown; rest > 0 {
			s += fmt.Sprintf(" and %d more", rest)
		}
		s += " not set"
		if len([]rune(s)) <= budget {
			return s
		}
	}
	return ""
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
