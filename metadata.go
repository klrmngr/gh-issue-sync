package main

import (
	"regexp"
	"strings"
)

// MetaField is one "key: value" line from a report's trailing metadata block.
type MetaField struct {
	Name  string
	Value string
}

// metaLine matches the list items the in-game reporter appends, e.g.
// "- **Steam ID (unverified):** [7656...](https://...)".
var metaLine = regexp.MustCompile(`^[-*]\s+\*\*(.+?):?\*\*:?\s*(.*)$`)

// splitReportMetadata separates a report's prose from the metadata block the
// in-game reporter appends after a trailing "---" rule.
//
// The tail is only treated as metadata when *every* non-empty line in it is a
// bolded key/value item. A person writing "---" as a rule in their own report
// therefore keeps their text intact, at the cost of occasionally leaving a
// genuine block inline — the safe direction to fail in, since the alternative
// is silently swallowing someone's bug description.
func splitReportMetadata(body string) (string, []MetaField) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")

	// Find the last horizontal rule.
	rule := -1
	for i, l := range lines {
		if t := strings.TrimSpace(l); t == "---" || t == "***" || t == "___" {
			rule = i
		}
	}
	if rule == -1 || rule == len(lines)-1 {
		return strings.TrimSpace(body), nil
	}

	var fields []MetaField
	for _, l := range lines[rule+1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		m := metaLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			// Something in the tail is not a metadata item, so this rule is
			// part of the prose. Leave the body exactly as it came.
			return strings.TrimSpace(body), nil
		}
		fields = append(fields, MetaField{
			Name:  strings.TrimSpace(m[1]),
			Value: strings.TrimSpace(m[2]),
		})
	}
	if len(fields) == 0 {
		return strings.TrimSpace(body), nil
	}
	return strings.TrimSpace(strings.Join(lines[:rule], "\n")), fields
}
