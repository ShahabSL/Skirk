package main

import (
	"strings"
	"testing"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func TestObjectShapeAndExpectedSets(t *testing.T) {
	ids := make([]string, objectCount)
	for i := range ids {
		ids[i] = "test-id-" + string(rune('A'+i%26)) + "-" + strings.Repeat("0", i/26)
	}
	prefix := "muxv4/list-page-probe-test/down/peer/run/"
	objects := makeObjects(prefix, ids)
	if len(objects) != 528 {
		t.Fatalf("objects = %d, want 528", len(objects))
	}
	for _, item := range objects {
		if !strings.HasPrefix(item.Name, prefix) || !strings.Contains(item.Name, "/"+item.Class+"/") {
			t.Fatalf("invalid name or class: %q", item.Name)
		}
		if item.Class == "p0" && !strings.HasSuffix(item.Name, ".mo1") {
			t.Fatalf("p0 metadata-only suffix absent: %q", item.Name)
		}
	}
	for _, test := range []struct {
		variant string
		want    int
	}{
		{"broad", 528}, {"legacy_p0", 16}, {"indexed_p0", 8},
		{"legacy_p1", 512}, {"indexed_p1", 504},
	} {
		if got := len(expectedFor(objects, test.variant)); got != test.want {
			t.Errorf("%s expected set = %d, want %d", test.variant, got, test.want)
		}
	}
}

func TestQueriesPreserveLegacyFallback(t *testing.T) {
	p := &probe{cfg: &skirk.Config{Drive: skirk.DriveConfig{FolderID: "test-folder"}}, prefix: "muxv4/list-page-probe-test/down/peer/run/"}
	for _, test := range []struct {
		variant string
		want    string
		avoid   string
	}{
		{"legacy_p0", "name contains '/p0/'", "properties has"},
		{"legacy_p1", "name contains '/p1/'", "properties has"},
		{"indexed_p0", "properties has { key='skirkClass' and value='p0' }", "name contains '/p0/'"},
		{"indexed_p1", "properties has { key='skirkClass' and value='p1' }", "name contains '/p1/'"},
	} {
		q, err := p.listQuery(test.variant)
		if err != nil || !strings.Contains(q, "'test-folder' in parents") || !strings.Contains(q, "name contains '"+p.prefix+"'") ||
			!strings.Contains(q, test.want) || strings.Contains(q, test.avoid) {
			t.Errorf("%s query = %q, err = %v", test.variant, q, err)
		}
	}
	if _, err := p.listQuery("invalid"); err == nil {
		t.Fatal("invalid query variant accepted")
	}
}

func TestAssessDistinguishesLegacyFalseMatchesFromIndexedLeak(t *testing.T) {
	objects := []object{
		{ID: "priority", Name: "muxv4/test/p0/message", Class: "p0", Tagged: true},
		{ID: "bulk", Name: "muxv4/test/p1/message", Class: "p1", Tagged: true},
	}
	files := []driveFile{{ID: "priority", Name: objects[0].Name}, {ID: "bulk", Name: objects[1].Name}}
	legacy := sweepResult{Variant: "legacy_p0"}
	assess(&legacy, files, expectedFor(objects, "legacy_p0"), "muxv4/test/")
	if legacy.Missing != 0 || legacy.Unexpected != 0 || legacy.LocallyMatched != 1 || legacy.Failure != "" {
		t.Fatalf("legacy assessment = %+v", legacy)
	}
	indexed := sweepResult{Variant: "indexed_p0"}
	assess(&indexed, files, expectedFor(objects, "indexed_p0"), "muxv4/test/")
	if indexed.Unexpected != 1 || indexed.Failure == "" {
		t.Fatalf("indexed assessment = %+v", indexed)
	}
}

func TestLiveFlagRequired(t *testing.T) {
	if _, err := parseOptions([]string{"--config", "x", "--out", "y.json"}); err == nil {
		t.Fatal("probe accepted invocation without --live")
	}
}
