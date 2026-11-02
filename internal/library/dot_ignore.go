// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// dotIgnoreFileName is the file that upstream DotIgnoreIgnoreRule
// (Emby.Server.Implementations/Library/DotIgnoreIgnoreRule.cs) looks for.
const dotIgnoreFileName = ".ignore"

// dotIgnoreRule ports upstream DotIgnoreIgnoreRule. For an entry, it finds
// the nearest .ignore file: in the entry itself for a directory, else in the
// entry's directory, then in each parent up to the filesystem root. Only the
// nearest file applies. An empty file, a file with only white space, a file
// whose target is missing, or a file without one valid pattern ignores every
// entry under it. Other files hold gitignore-style patterns that
// dotIgnoreRules.match evaluates against the full entry path.
//
// The rule caches the directory lookups and the parsed files. The scanner
// calls reset at the start of each scan, as upstream LibraryManager calls
// ClearIgnoreRuleCache in ValidateMediaLibraryInternal, so a scan sees the
// .ignore files that exist when it starts. A nil rule works without a cache.
type dotIgnoreRule struct {
	mu sync.Mutex
	// dirs maps a directory to the directory that holds its nearest .ignore
	// file, or to "" when there is none.
	dirs map[string]string
	// files maps a .ignore path to its parsed rules.
	files map[string]*dotIgnoreRules
}

func newDotIgnoreRule() *dotIgnoreRule {
	return &dotIgnoreRule{dirs: map[string]string{}, files: map[string]*dotIgnoreRules{}}
}

// reset clears the caches.
func (r *dotIgnoreRule) reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.dirs)
	clear(r.files)
}

// filter returns entries without the ones that a .ignore file excludes. It
// does not modify entries. A .ignore file that cannot be read is an error,
// as File.ReadAllText throws upstream.
func (r *dotIgnoreRule) filter(entries []Entry) ([]Entry, error) {
	return filterEntries(entries, r.shouldIgnore)
}

// shouldIgnore ports DotIgnoreIgnoreRule.IsIgnoredInternal.
func (r *dotIgnoreRule) shouldIgnore(entry Entry) (bool, error) {
	search := entry.Path
	if !entry.IsDir {
		search = filepath.Dir(entry.Path)
	}
	if search == "" {
		return false, nil
	}
	directory := r.ignoreDirectory(search)
	if directory == "" {
		return false, nil
	}
	rules, err := r.rules(filepath.Join(directory, dotIgnoreFileName))
	if err != nil || rules == nil {
		return false, err
	}
	if rules.ignoreAll {
		return true, nil
	}
	path := filepath.ToSlash(entry.Path)
	if entry.IsDir {
		// Upstream GetPathToCheck adds a trailing slash so that a
		// directory matches a "name/" pattern.
		path = strings.TrimRight(path, "/") + "/"
	}
	return rules.match(path), nil
}

// ignoreDirectory ports FindIgnoreFileCached: it returns the nearest
// directory at or above directory that holds a .ignore file, or "".
func (r *dotIgnoreRule) ignoreDirectory(directory string) string {
	var checked []string
	current := filepath.Clean(directory)
	for {
		if found, ok := r.cachedDirectory(current); ok {
			r.storeDirectories(checked, found)
			return found
		}
		checked = append(checked, current)
		if dotIgnoreFileExists(filepath.Join(current, dotIgnoreFileName)) {
			r.storeDirectories(checked, current)
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			r.storeDirectories(checked, "")
			return ""
		}
		current = parent
	}
}

func (r *dotIgnoreRule) cachedDirectory(directory string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.dirs[directory]
	return found, ok
}

func (r *dotIgnoreRule) storeDirectories(directories []string, found string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, directory := range directories {
		r.dirs[directory] = found
	}
}

// rules ports GetParsedRules and ParseIgnoreFile. It returns nil when the
// file no longer exists.
func (r *dotIgnoreRule) rules(path string) (*dotIgnoreRules, error) {
	if r != nil {
		r.mu.Lock()
		cached, ok := r.files[path]
		r.mu.Unlock()
		if ok {
			return cached, nil
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil
	}
	parsed := &dotIgnoreRules{ignoreAll: true}
	if info.Mode()&fs.ModeSymlink != 0 || info.Size() != 0 {
		content, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A link whose target is missing counts as an empty file.
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", path, err)
		default:
			parsed = parseDotIgnore(string(content))
		}
	}
	if r != nil {
		r.mu.Lock()
		r.files[path] = parsed
		r.mu.Unlock()
	}
	return parsed, nil
}

// dotIgnoreFileExists ports FileInfo.Exists: true for a file or for a link
// that does not resolve to a directory, including a link whose target is
// missing.
func dotIgnoreFileExists(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Stat(path)
		return err != nil || !target.IsDir()
	}
	return !info.IsDir()
}

// dotIgnoreRules is one parsed .ignore file.
type dotIgnoreRules struct {
	ignoreAll bool
	patterns  []dotIgnorePattern
}

// dotIgnorePattern is one rule of the upstream Ignore library (version
// 0.2.1, IgnoreRule.cs). A nil regex is a blank line or a comment, which
// matches nothing but counts as a valid rule.
type dotIgnorePattern struct {
	negate bool
	regex  *regexp.Regexp
}

// parseDotIgnore ports ParseIgnoreFile for a non-empty file.
func parseDotIgnore(content string) *dotIgnoreRules {
	if strings.TrimSpace(content) == "" {
		return &dotIgnoreRules{ignoreAll: true}
	}
	parsed := &dotIgnoreRules{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pattern, ok := compileDotIgnorePattern(line)
		if !ok {
			// Upstream skips a pattern that does not compile.
			continue
		}
		parsed.patterns = append(parsed.patterns, pattern)
	}
	parsed.ignoreAll = len(parsed.patterns) == 0
	return parsed
}

// match ports Ignore.IsPathIgnored: the first matching rule ignores the
// path, and a later matching negated rule includes it again.
func (r *dotIgnoreRules) match(path string) bool {
	ignored := false
	for _, pattern := range r.patterns {
		if pattern.regex == nil {
			continue
		}
		if pattern.negate {
			if ignored && pattern.regex.MatchString(path) {
				ignored = false
			}
		} else if !ignored && pattern.regex.MatchString(path) {
			ignored = true
		}
	}
	return ignored
}

// compileDotIgnorePattern ports the IgnoreRule constructor. It returns false
// when the translated expression does not compile.
func compileDotIgnorePattern(pattern string) (dotIgnorePattern, bool) {
	if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "#") {
		return dotIgnorePattern{}, true
	}
	var result dotIgnorePattern
	switch {
	case strings.HasPrefix(pattern, `\!`), strings.HasPrefix(pattern, `\#`):
		pattern = pattern[1:]
	case strings.HasPrefix(pattern, "!"):
		result.negate = true
		pattern = pattern[1:]
	}
	regex, err := regexp.Compile("(?i)" + translateDotIgnorePattern(pattern))
	if err != nil {
		return dotIgnorePattern{}, false
	}
	result.regex = regex
	return result, true
}

var (
	ignoreTrailingSpaces = regexp.MustCompile(`\\?\s+$`)
	ignoreEscapedSpace   = regexp.MustCompile(`\\\s`)
	ignoreLeadingStars   = regexp.MustCompile(`^(\*\*/|\*\*)`)
	ignoreMiddleSlash    = regexp.MustCompile(`^([^/\^]+/[^/]+)`)
	ignoreTrailingSlash  = regexp.MustCompile(`^([^/]+)/$`)
	ignoreLastSegment    = regexp.MustCompile(`([^/$]+)$`)
)

// translateDotIgnorePattern applies the ReplacerStash replacers of the
// upstream Ignore library in IgnoreRule.Replacers order. The steps work on
// the partly translated text, as upstream does, so a later step can see the
// output of an earlier one. Steps that upstream writes with look-around
// expressions, which RE2 does not support, are written as string scans.
func translateDotIgnorePattern(pattern string) string {
	// TrailingSpaces.
	pattern = ignoreTrailingSpaces.ReplaceAllStringFunc(pattern, func(match string) string {
		if strings.HasPrefix(match, `\`) {
			return " "
		}
		return ""
	})
	// EscapedSpaces.
	pattern = ignoreEscapedSpace.ReplaceAllLiteralString(pattern, " ")
	// LiteralDot and LiteralPlus.
	pattern = strings.ReplaceAll(pattern, ".", `\.`)
	pattern = strings.ReplaceAll(pattern, "+", `\+`)
	// QuestionMark: "(?!\\)\?" matches every "?".
	pattern = strings.ReplaceAll(pattern, "?", "[^/]")
	// NoSlash.
	if !strings.Contains(pattern, "/") {
		pattern = "(^|/)" + pattern
	}
	// NonLeadingSingleStar: "(?<!^)(?<!\*)\*(?!\*)".
	pattern = replaceSingleStars(pattern)
	// LeadingSingleStar: "^(?<!\*)\*(?!\*)".
	if strings.HasPrefix(pattern, "*") && !strings.HasPrefix(pattern, "**") {
		pattern = ".*" + pattern[1:]
	}
	// LeadingDoubleStar.
	pattern = ignoreLeadingStars.ReplaceAllLiteralString(pattern, ".*")
	// MiddleDoubleStar: "(?<=/)\*\*/".
	pattern = replaceMiddleDoubleStars(pattern)
	// LeadingSlash.
	if strings.HasPrefix(pattern, "/") {
		pattern = "^" + pattern[1:]
	}
	// TrailingDoubleStar.
	if strings.HasSuffix(pattern, "**") {
		pattern = pattern[:len(pattern)-2] + ".*$"
	}
	// OtherDoubleStar.
	pattern = strings.ReplaceAll(pattern, "**", "[^/]*")
	// MiddleSlash.
	pattern = ignoreMiddleSlash.ReplaceAllString(pattern, "^${1}")
	// TrailingSlash.
	pattern = ignoreTrailingSlash.ReplaceAllString(pattern, "(/|^)${1}/")
	// NoTrailingSlash.
	pattern = ignoreLastSegment.ReplaceAllString(pattern, "${1}(/.*)?$")
	// Ending.
	return ignoreLastSegment.ReplaceAllString(pattern, "${1}$")
}

// replaceSingleStars replaces each "*" that is not the first character and
// has no "*" next to it with "[^/]*".
func replaceSingleStars(pattern string) string {
	var builder strings.Builder
	for index := 0; index < len(pattern); index++ {
		if pattern[index] == '*' && index > 0 && pattern[index-1] != '*' && (index+1 == len(pattern) || pattern[index+1] != '*') {
			builder.WriteString("[^/]*")
			continue
		}
		builder.WriteByte(pattern[index])
	}
	return builder.String()
}

// replaceMiddleDoubleStars replaces each "**/" that follows a "/" with
// "(.*/)?".
func replaceMiddleDoubleStars(pattern string) string {
	var builder strings.Builder
	for index := 0; index < len(pattern); {
		if index > 0 && pattern[index-1] == '/' && strings.HasPrefix(pattern[index:], "**/") {
			builder.WriteString("(.*/)?")
			index += len("**/")
			continue
		}
		builder.WriteByte(pattern[index])
		index++
	}
	return builder.String()
}
