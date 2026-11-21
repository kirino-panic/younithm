package match

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	featRe     = regexp.MustCompile(`(?i)[\(\[]?\b(feat\.?|ft\.?|featuring|with)\b[^\)\]]*[\)\]]?`)
	remasterRe = regexp.MustCompile(`(?i)[\(\[\-–]\s*(\d{4}\s+)?(digital\s+)?remaster(ed)?(\s+\d{4})?\s*(version)?\s*[\)\]]?`)
	spaceRe    = regexp.MustCompile(`\s+`)
)

// Words that make a recording a different recording (live and remix
// versions are distinct). "remaster" is deliberately not here:
// it is stripped by NormTitle instead.
var versionMarkers = []string{"live", "remix", "acoustic", "instrumental", "demo", "karaoke", "cover"}

// fold lowercases, unifies "&"/"and", drops punctuation and collapses space.
func fold(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' || r == '’':
			// drop apostrophes: "don't" == "dont"
		default:
			b.WriteRune(' ')
		}
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(b.String(), " "))
}

// NormTitle normalises a track/album title for comparison (remaster tags
// removed; feature credits removed).
func NormTitle(s string) string {
	s = remasterRe.ReplaceAllString(s, " ")
	s = featRe.ReplaceAllString(s, " ")
	return fold(s)
}

// SearchTitle is a title as a catalogue would spell it for a search: the
// remaster tag and the feature credit that NormTitle ignores when comparing are
// left out, the rest (case, punctuation, version words) is kept as written.
func SearchTitle(s string) string {
	s = remasterRe.ReplaceAllString(s, " ")
	s = featRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// NormArtist normalises an artist string; "feat." tails are removed.
func NormArtist(s string) string {
	return fold(featRe.ReplaceAllString(s, " "))
}

func markers(norm string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(norm) {
		for _, v := range versionMarkers {
			if w == v {
				m[v] = true
			}
		}
	}
	return m
}

// Similarity returns 0..1 between two already-normalised strings: the better
// of edit-distance ratio and token-set overlap.
func Similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	lev := 1 - float64(levenshtein([]rune(a), []rune(b)))/float64(max(len([]rune(a)), len([]rune(b))))
	ta, tb := strings.Fields(a), strings.Fields(b)
	set := map[string]bool{}
	for _, t := range ta {
		set[t] = true
	}
	inter := 0
	seen := map[string]bool{}
	for _, t := range tb {
		if set[t] && !seen[t] {
			inter++
			seen[t] = true
		}
	}
	// Jaccard (overlap coefficient is too generous for 1-word subsets).
	uniqB := map[string]bool{}
	for _, t := range tb {
		uniqB[t] = true
	}
	jac := float64(inter) / float64(len(set)+len(uniqB)-inter)
	return max(lev, jac)
}

// TitleSimilarity compares titles, halving the score when exactly one side
// carries a version marker such as "live" or "remix".
func TitleSimilarity(a, b string) float64 {
	na, nb := NormTitle(a), NormTitle(b)
	s := Similarity(na, nb)
	ma, mb := markers(na), markers(nb)
	for k := range ma {
		if !mb[k] {
			return s / 2
		}
	}
	for k := range mb {
		if !ma[k] {
			return s / 2
		}
	}
	return s
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
