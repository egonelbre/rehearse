package main

import "testing"

func TestTrackName(t *testing.T) {
	for input, expect := range map[string]string{
		"_Soprano_1_(Voice_1).wav": "_Soprano_1",
		"_Soprano_1_(Voice_3).wav": "_Soprano_1",
		"Soprano 1 (Voice 2).wav":  "Soprano 1",
		"Soprano 1 (voice10).wav":  "Soprano 1",
		"_Soprano_2.wav":           "_Soprano_2",
		"Alto (div).wav":           "Alto (div)",
	} {
		if got := trackFromPath(input).Name; got != expect {
			t.Errorf("%q: got %q, want %q", input, got, expect)
		}
	}
}
