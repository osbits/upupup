package app

import "testing"

func TestPromLabelKeySanitisesDisallowedCharacters(t *testing.T) {
	tests := map[string]string{
		"env":        "env",
		"team-name":  "team_name",
		"9invalid":   "_invalid",
		"":           "_",
		"service.ok": "service_ok",
	}
	for input, expected := range tests {
		if got := promLabelKey(input); got != expected {
			t.Fatalf("promLabelKey(%q) = %q, expected %q", input, got, expected)
		}
	}
}

func TestPromLabelValueEscapesSpecialCharacters(t *testing.T) {
	input := "foo\"bar\nbaz\\"
	got := promLabelValue(input)
	expected := "foo\\\"bar\\nbaz\\\\"
	if got != expected {
		t.Fatalf("unexpected escaped value: %q (expected %q)", got, expected)
	}
}
