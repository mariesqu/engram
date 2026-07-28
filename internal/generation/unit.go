package generation

import (
	"sort"
	"strings"

	"github.com/mariesqu/engram/internal/domain"
)

// Unit is one narration unit: a project's slice of one topic prefix. See
// REQ-GEN-03 -- the unit of narration is scoped to (project, topic_prefix)
// and never a bare topic, because Phase A's topic hubs are inherently
// cross-project (byTopicPrefix is accumulated inside the per-project loop
// but iterated outside it, internal/obsidian/exporter.go:223-239,405). A
// bare per-topic unit would hand the generation gate one project's policy
// while the prompt contained another project's text -- a privacy-gate
// bypass, not a modelling convenience.
type Unit struct {
	// ProjectKey and TopicPrefixKey carry the record data's own casing
	// (not folded) so a rendered unit's identity matches what the exporter
	// already displays. Use UnitKey for lookups/comparisons.
	ProjectKey     string
	TopicPrefixKey string
	Records        []*domain.Record
	// InputChars is the combined character count of every member's Title
	// plus Content -- the same text BuildPrompt sends to the provider, so
	// it is what the eligibility threshold and the per-cycle char budget
	// (a later slice) both measure against.
	InputChars int
}

// UnitKey returns the case-folded identity key for a (project, prefix)
// pair: lower(project) + "\x00" + lower(prefix). Mirrors
// internal/localstore's narrativeFoldKey and internal/obsidian's
// NarrativeReader key exactly, so all three layers agree on identity for
// the same logical unit.
func UnitKey(project, prefix string) string {
	return strings.ToLower(project) + "\x00" + strings.ToLower(prefix)
}

// AssembleUnits groups records into narration units scoped by
// (project, topic prefix). The prefix is derived by splitting each
// record's TopicKey on the LAST "/", matching Phase A's REQ-EXPORT-05 rule
// (internal/obsidian.TopicPrefix, hub.go:102-108). The rule is duplicated
// here rather than shared because internal/generation must not import
// internal/obsidian (REQ-GEN-08 is a symmetric import boundary).
//
// A record with a nil TopicKey, or one with no "/", contributes to no unit
// -- it has no topic prefix to be narrated under.
//
// Units are returned sorted by their case-folded UnitKey, for deterministic
// output independent of map iteration order.
func AssembleUnits(recs []*domain.Record) []Unit {
	type bucket struct {
		project string
		prefix  string
		records []*domain.Record
	}

	buckets := map[string]*bucket{}
	var order []string

	for _, r := range recs {
		if r == nil || r.TopicKey == nil {
			continue
		}
		prefix, ok := topicPrefix(*r.TopicKey)
		if !ok {
			continue
		}
		key := UnitKey(r.Project, prefix)
		b, exists := buckets[key]
		if !exists {
			b = &bucket{project: r.Project, prefix: prefix}
			buckets[key] = b
			order = append(order, key)
		}
		b.records = append(b.records, r)
	}

	sort.Strings(order)

	units := make([]Unit, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		chars := 0
		for _, r := range b.records {
			chars += len(r.Title) + len(r.Content)
		}
		units = append(units, Unit{
			ProjectKey:     b.project,
			TopicPrefixKey: b.prefix,
			Records:        b.records,
			InputChars:     chars,
		})
	}
	return units
}

// topicPrefix splits a topic key on the LAST "/", mirroring
// internal/obsidian.TopicPrefix (hub.go:102-108) exactly.
func topicPrefix(topicKey string) (prefix string, ok bool) {
	i := strings.LastIndex(topicKey, "/")
	if i < 0 {
		return "", false
	}
	return topicKey[:i], true
}

// Eligible reports whether the unit has enough material to be narrated.
// REQ-NARR-04: a unit is eligible only when it has BOTH at least minObs
// observations AND at least minChars combined InputChars -- a unit below
// either threshold must not be sent to the provider and must not occupy a
// cache row.
func (u Unit) Eligible(minObs, minChars int) bool {
	return len(u.Records) >= minObs && u.InputChars >= minChars
}
