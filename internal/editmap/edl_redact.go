package editmap

import (
	"fmt"

	"github.com/self-deprecated/agent-reel/internal/redact"
)

// EDL `redact` rules (EDL.md, "Redaction"): a top-level list of
// {"preset":"secrets"|"emails"|"homepaths"} and {"pattern":RE2,"replace":…}
// entries, strictly decoded and validated by internal/redact. The resolver
// only canonicalises them; the render masks matching cells in every chunk's
// repaint projection.

func decodeRedact(top edlObject) ([]redact.Rule, error) {
	list, ok, err := top.array("redact", redact.MaxRules)
	if err != nil || !ok {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("edl: redact lists no rules; omit it")
	}
	rules := make([]redact.Rule, 0, len(list))
	for i, raw := range list {
		where := fmt.Sprintf("redact rule %d", i+1)
		obj, err := edlParseObject(raw, where)
		if err != nil {
			return nil, err
		}
		if err := obj.allow("preset", "pattern", "replace"); err != nil {
			return nil, err
		}
		var r redact.Rule
		if r.Preset, _, err = obj.str("preset"); err != nil {
			return nil, err
		}
		if r.Pattern, _, err = obj.str("pattern"); err != nil {
			return nil, err
		}
		if r.Replace, _, err = obj.str("replace"); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	out, err := redact.Canonical(rules)
	if err != nil {
		return nil, fmt.Errorf("edl: %w", err)
	}
	return out, nil
}
