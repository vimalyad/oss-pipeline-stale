package guard

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// CommitPatterns are the rules that block a commit message, compiled from
// config/forbidden-trailers.txt.
//
// The same file the git hooks read, deliberately. Two enforcers, one source of
// truth: the hook catches anything committed by hand or by a tool that is not
// this pipeline, and this catches the case the hook cannot -- a clone whose
// core.hooksPath was rewritten, or a hook script that is simply missing.
// Duplicating the patterns in Go would let the two drift, and the one that
// drifted would be the one nobody was watching.
type CommitPatterns struct {
	res  []*regexp.Regexp
	srcs []string
}

// LoadCommitPatterns reads the pattern file. A missing or empty file is an
// error rather than a permissive default: the rule it encodes is absolute, and
// silently allowing everything is the worst possible reading of "the file is
// not there".
func LoadCommitPatterns(path string) (*CommitPatterns, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("forbidden-trailers: %w", err)
	}
	defer f.Close()

	p := &CommitPatterns{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		// (?i) and (?m) to match `grep -iE` over a multi-line message: the
		// patterns anchored with ^ mean "at the start of any line", which is
		// how a trailer appears.
		re, err := regexp.Compile("(?im)" + text)
		if err != nil {
			return nil, fmt.Errorf("forbidden-trailers:%d: %w", line, err)
		}
		p.res = append(p.res, re)
		p.srcs = append(p.srcs, text)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("forbidden-trailers: %w", err)
	}
	if len(p.res) == 0 {
		return nil, fmt.Errorf("forbidden-trailers: %s defines no patterns", path)
	}
	return p, nil
}

// Check returns what a commit message matched, phrased so the offending text
// and the rule that caught it are both visible. Empty means the message is
// clear to commit.
func (p *CommitPatterns) Check(message string) []string {
	var out []string
	for i, re := range p.res {
		if m := re.FindString(message); m != "" {
			out = append(out, fmt.Sprintf("%q matches %s", strings.TrimSpace(m), p.srcs[i]))
		}
	}
	return out
}
