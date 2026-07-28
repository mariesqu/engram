package generation

import (
	"fmt"
	"strings"
)

// promptTemplateVersion identifies this prompt's shape for the content-hash
// cache key (REQ-NARR-01): folded into SourceHash so that changing the
// template text invalidates every existing cache entry on the next cycle,
// rather than silently serving prose generated under a different prompt.
const promptTemplateVersion = "pt-1"

// BuildPrompt assembles the chat prompt for one narrative unit. It is a
// PURE function of u.Records: no store access, no network call, and no
// data source beyond what the caller placed in the Unit. That purity is
// what keeps TestBuildPromptContainsNoForeignProjectText meaningful -- it
// restates REQ-GEN-03's privacy-gate-bypass guard at the prompt layer,
// where a leak would be the most direct route for one project's text to
// reach another project's rendered narrative.
//
// The prompt instructs the model to write every claim as RECORDED by the
// source observations, never as independently verified fact. REQ-PROV-05
// exists because there is no filesystem, process, or test-runner oracle in
// this change's scope capable of checking a completion or behavioural
// claim (e.g. "27/27 tasks done", "39 tests green") -- attribution voice
// here, plus the Go-authored banner applied later entirely outside the
// model's control (REQ-PROV-04), are the only two mitigations; neither is
// verification.
func BuildPrompt(u Unit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are synthesising a narrative summary from %d recorded observations "+
		"for the topic %q.\n\n", len(u.Records), u.TopicPrefixKey)

	b.WriteString("Every claim below was RECORDED by a human or a tool at the time noted; it " +
		"was never independently verified by you. Write your summary the same way: state what " +
		"was recorded, never assert it as an independently confirmed fact. In particular, treat " +
		"any recorded claim about task completion, test results, or other behavioural outcomes " +
		"(for example \"tests pass\", \"N/N tasks done\") as a claim someone recorded, not as " +
		"something you have verified -- you have no way to check it.\n\n")

	b.WriteString("Source observations:\n")
	for i, r := range u.Records {
		fmt.Fprintf(&b, "%d. Title: %s\nContent: %s\n\n", i+1, r.Title, r.Content)
	}

	return b.String()
}
