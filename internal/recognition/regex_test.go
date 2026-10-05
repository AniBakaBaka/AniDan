// SPDX-License-Identifier: AGPL-3.0-only
package recognition

import (
	"strings"
	"testing"
	"time"
)

func TestLegacyLookaround(t *testing.T) {
	for _, tc := range []struct {
		pattern, text string
		want          bool
	}{
		{`^(?!.*第[0-9]+集).*$`, "完美世界 第268集", false},
		{`^(?!.*第[0-9]+集).*$`, "完美世界 预告", true},
		{`预告(?!片)`, "完美世界 预告", true},
		{`预告(?!片)`, "完美世界 预告片", false},
		{`(?<=第)[0-9]+(?=集)`, "完美世界 第268集", true},
		{`\bOP(?!EN)\b`, "[OP]", true},
		{`\bOP(?!EN)\b`, "OPEN", false},
		{`(?P<episode>[0-9]+)(?!预告)集`, "第268集", true},
	} {
		re, err := CompileRegex(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		got, err := re.MatchString(tc.text)
		if err != nil || got != tc.want {
			t.Fatalf("%q on %q: %v, %v", tc.pattern, tc.text, got, err)
		}
	}
}

func TestRegexByteOffsetsAndCase(t *testing.T) {
	re, err := CompileRegex(`(?<=第)[0-9]+(?=集)`)
	if err != nil {
		t.Fatal(err)
	}
	text := "完美世界 第268集 第269集"
	matches, err := re.FindAllStringIndex(text, -1)
	if err != nil || len(matches) != 2 || text[matches[0][0]:matches[0][1]] != "268" || text[matches[1][0]:matches[1][1]] != "269" {
		t.Fatalf("bad UTF-8 offsets: %v, %v", matches, err)
	}
	for _, pattern := range []string{`OP`, `OP(?!EN)`} {
		for _, insensitive := range []bool{false, true} {
			re, err := CompileRegexCase(pattern, insensitive)
			if err != nil {
				t.Fatal(err)
			}
			got, err := re.MatchString("op")
			if err != nil || got != insensitive {
				t.Fatal("case policy changed")
			}
		}
	}
	re, _ = CompileRegex(`第[0-9]+集`)
	if re.linear == nil {
		t.Fatal("ordinary rules must keep the native engine")
	}
}

func TestRegexLimitsAndTimeout(t *testing.T) {
	if _, err := CompileRegex(strings.Repeat("a", 16385)); err == nil {
		t.Fatal("pattern limit bypassed")
	}
	if _, err := CompileRegex(`(?!`); err == nil {
		t.Fatal("invalid pattern accepted")
	}
	re, err := CompileRegex(`^(?!z)(a+)+$`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := re.MatchString(strings.Repeat("a", MaxText+1)); err == nil {
		t.Fatal("text limit bypassed")
	}
	started := time.Now()
	if _, err := re.MatchString(strings.Repeat("a", 4096) + "!"); err == nil {
		t.Fatal("backtracking timeout was ignored")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("matching did not stop promptly")
	}
}

func TestLegacyEnglishBlacklistBoundary(t *testing.T) {
	b, err := NewBlacklist("", `OP(?!EN)`)
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]bool{"完美世界 [op01]": true, "OPEN": false, "SCOPE": false} {
		got, err := b.Match(text)
		if err != nil || got != want {
			t.Fatalf("%q: %v, %v", text, got, err)
		}
	}
}
