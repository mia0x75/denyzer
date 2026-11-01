package stringcache

import (
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sirupsen/logrus"

	"github.com/mia0x75/denyzer/log"
)

type stringCache interface {
	elementCount() int
	// findMatch reports whether the cache matches searchString and, if so,
	// returns the rule that matched. rule is empty when ok is false.
	findMatch(searchString string) (rule string, ok bool)
}

type cacheFactory interface {
	addEntry(entry string) bool
	create() stringCache
	count() int
}

// logMatch runs on every hit, so it skips the logrus entry and argument
// allocations unless debug logging is on.
func logMatch(prefix, kind, rule, searchString string) {
	if log.Log().IsLevelEnabled(logrus.DebugLevel) {
		log.PrefixedLog(prefix).Debugf("%s '%s' matched with '%s'", kind, rule, searchString)
	}
}

// hashCache matches exact, case-insensitive strings. It stores only their
// hashes; the matched rule is the lowercased search string.
type hashCache struct {
	seed uint64
	set  hashSet
}

func (c hashCache) elementCount() int {
	return c.set.len()
}

func (c hashCache) findMatch(searchString string) (string, bool) {
	if searchString == "" || !c.set.contains(hashFold(c.seed, searchString)) {
		return "", false
	}

	rule := strings.ToLower(searchString)
	logMatch("string_map", "block rule", rule, searchString)

	return rule, true
}

type stringCacheFactory struct {
	entries  hashedEntries
	accepted int
}

func newStringCacheFactory() cacheFactory {
	return &stringCacheFactory{entries: newHashedEntries()}
}

// count returns the number of accepted entries, including duplicates.
func (f *stringCacheFactory) count() int {
	return f.accepted
}

func (f *stringCacheFactory) addEntry(entry string) bool {
	if len(entry) == 0 {
		return true // invalid but handled
	}

	f.entries.add(hashFold(f.entries.seed, entry))
	f.accepted++

	return true
}

func (f *stringCacheFactory) create() stringCache {
	if f.accepted == 0 {
		return nil
	}

	return hashCache{seed: f.entries.seed, set: f.entries.build()}
}

type regexEntry struct {
	re       *regexp.Regexp
	required string
}

type regexCache []regexEntry

func (c regexCache) elementCount() int {
	return len(c)
}

func (c regexCache) findMatch(searchString string) (string, bool) {
	for i := range c {
		regex := &c[i]
		if regex.required != "" && !strings.Contains(searchString, regex.required) {
			continue
		}

		if regex.re.MatchString(searchString) {
			logMatch("regex_cache", "regex", regex.re.String(), searchString)

			// re-wrap in the '/.../' delimiters that addEntry strips on insertion
			// so the reported rule matches the entry as configured by the user.
			return "/" + regex.re.String() + "/", true
		}
	}

	return "", false
}

type regexCacheFactory struct {
	cache regexCache
}

func (f *regexCacheFactory) addEntry(entry string) bool {
	// A regex entry is delimited by a leading and a trailing slash (/regex/), so
	// it needs at least those two characters. Without the length guard a lone
	// "/" satisfies both HasPrefix and HasSuffix and then panics below, where the
	// delimiters are stripped (entry[1:len-1] would be "/"[1:0]).
	if len(entry) < 2 || !strings.HasPrefix(entry, "/") || !strings.HasSuffix(entry, "/") {
		return false
	}

	// Trim slashes
	entry = strings.TrimSpace(entry[1 : len(entry)-1])

	compile, err := regexp.Compile(entry)
	if err != nil {
		log.Log().Warnf("invalid regex '%s'", entry)

		return true // invalid but handled
	}

	f.cache = append(f.cache, regexEntry{re: compile, required: regexRequiredLiteral(entry)})

	return true
}

// regexRequiredLiteral returns a literal present in every possible match, or
// empty to leave the pattern on the unfiltered standard-library path. Parsing
// happens only on insertion; lookup never parses or allocates filter state.
func regexRequiredLiteral(pattern string) string {
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return ""
	}

	return requiredLiteral(tree)
}

func requiredLiteral(tree *syntax.Regexp) string {
	switch tree.Op {
	case syntax.OpLiteral:
		if tree.Flags&syntax.FoldCase != 0 {
			return ""
		}

		// regexp treats malformed UTF-8 as RuneError. A byte substring check
		// cannot prove absence of that rune in arbitrary query strings.
		if slices.Contains(tree.Rune, utf8.RuneError) {
			return ""
		}

		return string(tree.Rune)
	case syntax.OpCapture, syntax.OpPlus:
		return requiredLiteral(tree.Sub[0])
	case syntax.OpRepeat:
		if tree.Min > 0 {
			return requiredLiteral(tree.Sub[0])
		}
	case syntax.OpConcat:
		var longest string
		for _, sub := range tree.Sub {
			if literal := requiredLiteral(sub); len(literal) > len(longest) {
				longest = literal
			}
		}

		return longest
	case syntax.OpNoMatch,
		syntax.OpEmptyMatch,
		syntax.OpCharClass,
		syntax.OpAnyCharNotNL,
		syntax.OpAnyChar,
		syntax.OpBeginLine,
		syntax.OpEndLine,
		syntax.OpBeginText,
		syntax.OpEndText,
		syntax.OpWordBoundary,
		syntax.OpNoWordBoundary,
		syntax.OpStar,
		syntax.OpQuest,
		syntax.OpAlternate:
		// Alternation, optional/zero repetitions, assertions, character
		// classes, and empty-match operations deliberately fall back to no
		// filter.
		return ""
	default:
		// Keep unknown future syntax operations conservative.
		return ""
	}

	return ""
}

func (f *regexCacheFactory) count() int {
	return len(f.cache)
}

func (f *regexCacheFactory) create() stringCache {
	if len(f.cache) == 0 {
		return nil
	}

	return f.cache
}

func newRegexCacheFactory() cacheFactory {
	return &regexCacheFactory{
		cache: make(regexCache, 0),
	}
}
