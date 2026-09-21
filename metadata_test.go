package main

import "testing"

const inGameBody = `The deck importer seems to be down, oddly enough that creates a bug.

---
- **Reporter:** Suspicious Entity (White)
- **Steam ID (unverified):** [76561198027578778](https://steamcommunity.com/profiles/76561198027578778)
- **Table version:** v0.2.12
- **Filed via:** in-game report button`

func TestSplitReportMetadata(t *testing.T) {
	body, fields := splitReportMetadata(inGameBody)

	if body != "The deck importer seems to be down, oddly enough that creates a bug." {
		t.Errorf("prose not cleanly separated:\n%q", body)
	}
	want := []MetaField{
		{"Reporter", "Suspicious Entity (White)"},
		{"Steam ID (unverified)", "[76561198027578778](https://steamcommunity.com/profiles/76561198027578778)"},
		{"Table version", "v0.2.12"},
		{"Filed via", "in-game report button"},
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(fields), len(want), fields)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, fields[i], want[i])
		}
	}
}

func TestSplitReportMetadataPlainIssue(t *testing.T) {
	body := "Just a normal issue someone typed on GitHub.\n\nWith two paragraphs."
	got, fields := splitReportMetadata(body)
	if got != body {
		t.Errorf("plain body was altered:\n%q", got)
	}
	if fields != nil {
		t.Errorf("plain body produced fields: %+v", fields)
	}
}

// A person writing "---" as a horizontal rule must not have the text after it
// swallowed into metadata. Failing to extract is recoverable; eating someone's
// bug description is not.
func TestSplitReportMetadataLeavesProseAlone(t *testing.T) {
	for _, body := range []string{
		"Here is the bug.\n\n---\n\nAnd here is more detail about it.",
		"Steps:\n\n---\n- **Reporter:** me\nthen some trailing prose",
		"Trailing rule with nothing after it.\n\n---",
		"---\n\nstarts with a rule then prose",
	} {
		got, fields := splitReportMetadata(body)
		if fields != nil {
			t.Errorf("prose misread as metadata: %q -> %+v", body, fields)
		}
		if got == "" {
			t.Errorf("body emptied: %q", body)
		}
	}
}

func TestSplitReportMetadataHandlesCRLF(t *testing.T) {
	_, fields := splitReportMetadata("Report text.\r\n\r\n---\r\n- **Reporter:** Someone\r\n- **Filed via:** button")
	if len(fields) != 2 {
		t.Fatalf("CRLF body gave %d fields, want 2: %+v", len(fields), fields)
	}
	if fields[0].Value != "Someone" {
		t.Errorf("value kept a stray carriage return: %q", fields[0].Value)
	}
}

func TestMetadataEmbedFields(t *testing.T) {
	issue := Issue{Number: 146, Body: inGameBody}
	issue.User.Login = "klrmngr"
	issue.Labels = []struct {
		Name string `json:"name"`
	}{{Name: "bug"}, {Name: "in-game-report"}}

	e := metadataEmbed(issue)
	if e == nil {
		t.Fatal("expected a metadata embed")
	}
	byName := map[string]string{}
	for _, f := range e.Fields {
		byName[f.Name] = f.Value
	}
	for name, want := range map[string]string{
		"Source":    "In-game report",
		"Opened by": "klrmngr",
		"Reporter":  "Suspicious Entity (White)",
		"Labels":    "bug, in-game-report",
	} {
		if byName[name] != want {
			t.Errorf("field %q = %q, want %q", name, byName[name], want)
		}
	}
	if len(e.Fields) > 25 {
		t.Errorf("embed has %d fields, Discord allows 25", len(e.Fields))
	}
}

// The starter message must no longer carry the metadata block.
func TestThreadEmbedExcludesMetadata(t *testing.T) {
	issue := Issue{Number: 146, Body: inGameBody, HTMLURL: "https://example.test/146"}
	e := threadEmbed(issue)
	for _, unwanted := range []string{"Suspicious Entity", "76561198027578778", "Filed via"} {
		if contains(e.Description, unwanted) {
			t.Errorf("starter message still contains metadata %q:\n%s", unwanted, e.Description)
		}
	}
	if !contains(e.Description, "deck importer seems to be down") {
		t.Errorf("starter message lost the report text:\n%s", e.Description)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
