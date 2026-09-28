// Package i18n translates the human-readable text the API sends (error
// messages, host/disk reasons, warnings, alert texts, job errors, SSE
// events) into English for a UI set to English.
//
// The server speaks Spanish: its messages are part of the API contract and
// stay as they are for every other client. Translation happens once, on the
// way out (see httpapi's language middleware), from a catalogue keyed by the
// exact Spanish format strings in the code: "%s es un volumen físico LVM"
// becomes a pattern whose %s is captured and put back verbatim in the English
// template. Messages are built from nested pieces ("disco en uso: " + reason
// + hint), so every entry is applied over the whole text, longest first, and
// each piece is translated by its own entry. TestCatalogueCoversTheCode
// fails when a user-visible Spanish string in the code has no entry.
package i18n

import (
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type entry struct {
	key   string
	re    *regexp.Regexp
	en    []piece
	probe string // the longest literal run of the key: a cheap pre-check
	whole bool   // a single word: translated only when it is the whole text
	lead  bool   // the pattern starts with a captured word boundary
	trail bool   // … and ends with one
}

func startsWithLetter(s string) bool {
	for _, r := range s {
		return unicode.IsLetter(r)
	}
	return false
}

func endsWithLetter(s string) bool {
	r := []rune(s)
	return len(r) > 0 && unicode.IsLetter(r[len(r)-1])
}

// piece — a literal run of the English template, or a reference to the
// Spanish capture it puts back (arg ≥ 0).
type piece struct {
	lit string
	arg int
}

var reVerb = regexp.MustCompile(`%(\[(\d+)\])?[-+# 0]*\d*(\.\d+)?([vsdqwfxXtc%])`)

var (
	once    sync.Once
	entries []*entry
)

func compile() {
	for es, en := range catalogue {
		e, err := compileEntry(es, en)
		if err != nil {
			log.Printf("i18n: entrada no válida %q: %v", es, err)
			continue
		}
		entries = append(entries, e)
	}
	// Longest first, so a whole sentence is translated before a fragment of
	// it could be; ties broken by key for a stable order.
	sort.Slice(entries, func(i, j int) bool {
		li, lj := literalLen(entries[i].key), literalLen(entries[j].key)
		if li != lj {
			return li > lj
		}
		return entries[i].key < entries[j].key
	})
}

func literalLen(k string) int { return len(reVerb.ReplaceAllString(k, "")) }

func compileEntry(es, en string) (*entry, error) {
	var re strings.Builder
	re.WriteString(`(?s)`)
	// A key that starts or ends with a letter matches only as whole words:
	// "montar %s: %v" must not rewrite the middle of "desmontar tank/a: …".
	// The boundary character is captured and put back (RE2 has no lookaround).
	lead := startsWithLetter(es)
	if lead {
		re.WriteString(`(^|[^\p{L}\p{N}_])`)
	}
	locs := reVerb.FindAllStringSubmatchIndex(es, -1)
	prev, probe := 0, ""
	nverbs := 0
	for _, l := range locs {
		if es[l[8]:l[9]] != "%" {
			nverbs++
		}
	}
	seen := 0
	for _, l := range locs {
		lit := es[prev:l[0]]
		if len(lit) > len(probe) {
			probe = lit
		}
		re.WriteString(regexp.QuoteMeta(lit))
		prev = l[1]
		switch verb := es[l[8]:l[9]]; verb {
		case "%":
			re.WriteString(`%`)
		case "d":
			re.WriteString(`(-?\d+)`)
			seen++
		case "f":
			re.WriteString(`(-?[\d.]+)`)
			seen++
		case "q":
			re.WriteString(`("(?:[^"\\]|\\.)*")`)
			seen++
		default:
			seen++
			if seen == nverbs && prev == len(es) {
				re.WriteString(`(.+)`) // nothing after it to stop a lazy match
			} else {
				re.WriteString(`(.+?)`)
			}
		}
	}
	tail := es[prev:]
	if len(tail) > len(probe) {
		probe = tail
	}
	re.WriteString(regexp.QuoteMeta(tail))
	trail := tail != "" && endsWithLetter(tail)
	if trail {
		re.WriteString(`($|[^\p{L}\p{N}_])`)
	}
	whole := nverbs == 0 && !strings.ContainsAny(strings.TrimSpace(es), " ")
	pat := re.String()
	if whole {
		pat = `^` + pat + `$`
	}
	rx, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	// English template: the same verbs, in order unless indexed (%[2]s).
	var pieces []piece
	prev, next := 0, 0
	for _, l := range reVerb.FindAllStringSubmatchIndex(en, -1) {
		if lit := en[prev:l[0]]; lit != "" {
			pieces = append(pieces, piece{lit: lit, arg: -1})
		}
		prev = l[1]
		if en[l[8]:l[9]] == "%" {
			pieces = append(pieces, piece{lit: "%", arg: -1})
			continue
		}
		arg := next
		if l[4] >= 0 {
			n, _ := strconv.Atoi(en[l[4]:l[5]])
			arg = n - 1
		}
		next = arg + 1
		pieces = append(pieces, piece{arg: arg})
	}
	if lit := en[prev:]; lit != "" {
		pieces = append(pieces, piece{lit: lit, arg: -1})
	}
	return &entry{key: es, re: rx, en: pieces, probe: probe, whole: whole, lead: lead, trail: trail}, nil
}

func (e *entry) enLiteral() string {
	var b strings.Builder
	for _, p := range e.en {
		b.WriteString(p.lit)
	}
	return b.String()
}

func (e *entry) apply(s string) string {
	if e.whole {
		// A single word: the whole text, or the head of a wrapped error
		// ("conflicto: …", from fmt.Errorf("%w: …", ErrConflict)).
		switch {
		case s == e.key:
			return e.enLiteral()
		case strings.HasPrefix(s, e.key+":"):
			return e.enLiteral() + s[len(e.key):]
		}
		return s
	} else if !strings.Contains(s, e.probe) {
		return s
	}
	return e.re.ReplaceAllStringFunc(s, func(m string) string {
		sub := e.re.FindStringSubmatch(m)
		args := sub[1:]
		var b strings.Builder
		if e.lead {
			b.WriteString(args[0])
			args = args[1:]
		}
		var after string
		if e.trail {
			after = args[len(args)-1]
			args = args[:len(args)-1]
		}
		for _, p := range e.en {
			if p.arg < 0 {
				b.WriteString(p.lit)
			} else if p.arg < len(args) {
				b.WriteString(args[p.arg])
			}
		}
		b.WriteString(after)
		return b.String()
	})
}

var (
	cacheMu sync.Mutex
	cache   = map[string]string{}
	missed  = map[string]bool{}
)

const cacheMax = 8192

// English translates s; text with nothing to translate comes back as it is.
func English(s string) string {
	if s == "" || !hasLetters(s) {
		return s
	}
	once.Do(compile)
	cacheMu.Lock()
	if v, ok := cache[s]; ok {
		cacheMu.Unlock()
		return v
	}
	cacheMu.Unlock()
	out := s
	for _, e := range entries {
		out = e.apply(out)
	}
	cacheMu.Lock()
	if len(cache) >= cacheMax {
		cache = map[string]string{}
	}
	cache[s] = out
	// Observability: Spanish that reached an English response untranslated
	// (a new message, or one built at run time) is logged once.
	if looksSpanish(out) && !missed[out] {
		if len(missed) >= 1024 {
			missed = map[string]bool{} // start over rather than go quiet
		}
		missed[out] = true
		log.Printf("i18n: sin traducción al inglés: %q", out)
	}
	cacheMu.Unlock()
	return out
}

func hasLetters(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

var reSpanishMark = regexp.MustCompile(`[áéíóúñ¿¡]`)

func looksSpanish(s string) bool { return reSpanishMark.MatchString(s) }
