// Package redact holds agent-reel's minimal redaction rules (course-design
// memo §4.3): presets and RE2 patterns evaluated on the text of one terminal
// row. It is pure (no I/O, no store access): callers feed it row text, get
// rune spans back and mask the matching cells in the repaint projection, so
// every derived output (video, still, proxy, clip casts, web embed) shows the
// same masking and the stored recording is never changed.
//
// Rules are evaluated per row. A secret wrapped over two terminal rows is not
// found; nor is a secret split across a PEM block's header (only its body
// lines are masked, one row at a time).
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rule is one redaction rule as an EDL `redact` entry (or a --redact preset)
// declares it: exactly one of Preset or Pattern. Replace is Mask ("█", the
// default), which fills every matched cell, or substitution text that takes
// the match's place: the rest of the row moves left (a substitution longer
// than the match is truncated to the matched width). A preset's replacement
// is its own unless overridden (Mask; "~" for homepaths).
type Rule struct {
	Preset  string `json:"preset,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Replace string `json:"replace,omitempty"`
}

// Presets.
const (
	PresetSecrets   = "secrets"
	PresetEmails    = "emails"
	PresetHomepaths = "homepaths"
)

// Mask is the default replacement: a full block, one cell wide.
const Mask = "█"

// Bounds.
const (
	MaxRules        = 16
	MaxPatternBytes = 512
	MaxReplaceRunes = 32
)

// SecretGroup is the capture group name that narrows a match to the part to
// mask (the value of key=value, the token after "Bearer"). Without it the
// whole match is masked.
const SecretGroup = "secret"

// Presets lists the preset names in documentation order.
func Presets() []string { return []string{PresetSecrets, PresetEmails, PresetHomepaths} }

// Label names rule number i (0-based) in reports and metadata without its
// pattern text, which may itself be a secret: "preset:secrets" or
// "pattern #2 (sha256:ab12cd34)" (the first 8 hex digits of the pattern's
// SHA-256).
func (r Rule) Label(i int) string {
	if r.Preset != "" {
		return "preset:" + r.Preset
	}
	sum := sha256.Sum256([]byte(r.Pattern))
	return fmt.Sprintf("pattern #%d (sha256:%s)", i+1, hex.EncodeToString(sum[:4]))
}

// Public returns rules safe to publish in plans and metadata: each pattern
// is replaced by its Label ("<withheld: pattern #2 (sha256:ab12cd34)>").
// Presets and replacements are kept. The result is for reading only; it does
// not compile to the same rules.
func Public(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	out := make([]Rule, len(rules))
	for i, r := range rules {
		if r.Pattern != "" {
			r.Pattern = "<withheld: " + r.Label(i) + ">"
		}
		out[i] = r
	}
	return out
}

type matcher struct {
	re    *regexp.Regexp
	group int
	// check, when set, must accept the masked text.
	check func(string) bool
}

type compiled struct {
	rule     Rule
	replace  string
	matchers []matcher
}

// Set is a compiled, validated list of rules.
type Set struct{ rules []compiled }

// Span is one masked rune range [Start, End) of a row's text and its
// replacement. Rules lists the indexes of the rules that matched inside it.
type Span struct {
	Start, End int
	Replace    string
	Rules      []int
}

// Fill reports whether the span masks cell by cell (Mask) rather than
// substituting its Replace text.
func (s Span) Fill() bool { return s.Replace == Mask }

// ParsePresets reads a --redact flag value: comma-separated preset names.
func ParsePresets(list string) ([]Rule, error) {
	var rules []Rule
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(Presets(), name) {
			return nil, fmt.Errorf("redact: unknown preset %q (presets: %s)", name, strings.Join(Presets(), ", "))
		}
		r := Rule{Preset: name}
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("redact: name at least one preset (%s)", strings.Join(Presets(), ", "))
	}
	return rules, nil
}

// Merge appends the rules of extra that rules does not already hold.
func Merge(rules, extra []Rule) []Rule {
	out := slices.Clone(rules)
	for _, r := range extra {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// Canonical validates rules and returns their canonical form: a pattern
// rule always carries its replacement, a preset only an explicit override
// that differs from the preset's own. nil stays nil.
func Canonical(rules []Rule) ([]Rule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	set, err := Compile(rules)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, len(set.rules))
	for i, c := range set.rules {
		r := c.rule
		if r.Preset != "" {
			if r.Replace == presetReplace(r.Preset) {
				r.Replace = ""
			}
		} else {
			r.Replace = c.replace
		}
		out[i] = r
	}
	return out, nil
}

// Compile validates and compiles rules (at most MaxRules).
func Compile(rules []Rule) (*Set, error) {
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("redact: %d rules; the limit is %d", len(rules), MaxRules)
	}
	s := &Set{}
	for i, r := range rules {
		where := fmt.Sprintf("redact rule %d", i+1)
		c := compiled{rule: r}
		switch {
		case r.Preset != "" && r.Pattern != "":
			return nil, fmt.Errorf("%s: preset and pattern are exclusive", where)
		case r.Preset != "":
			m, ok := presetMatchers[r.Preset]
			if !ok {
				return nil, fmt.Errorf("%s: unknown preset %q (presets: %s)", where, r.Preset, strings.Join(Presets(), ", "))
			}
			c.matchers = m
			c.replace = presetReplace(r.Preset)
		case r.Pattern != "":
			if len(r.Pattern) > MaxPatternBytes {
				return nil, fmt.Errorf("%s: pattern exceeds %d bytes", where, MaxPatternBytes)
			}
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				return nil, fmt.Errorf("%s: pattern is not an RE2 regexp: %w", where, err)
			}
			if re.MatchString("") {
				return nil, fmt.Errorf("%s: pattern %q matches empty text", where, r.Pattern)
			}
			c.matchers = []matcher{{re: re, group: re.SubexpIndex(SecretGroup)}}
			c.replace = Mask
		default:
			return nil, fmt.Errorf("%s: needs preset or pattern", where)
		}
		if r.Replace != "" {
			if err := validReplace(r.Replace); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			c.replace = r.Replace
		}
		if slices.ContainsFunc(s.rules, func(o compiled) bool { return o.rule == r }) {
			return nil, fmt.Errorf("%s repeats an earlier rule", where)
		}
		s.rules = append(s.rules, c)
	}
	return s, nil
}

func validReplace(s string) error {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n > MaxReplaceRunes {
		return fmt.Errorf("replace must be 1..%d characters", MaxReplaceRunes)
	}
	for _, r := range s {
		// One terminal cell per character: no controls, combining marks,
		// format characters or wide scripts.
		if !unicode.IsPrint(r) || unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana) || (r >= 0xFF01 && r <= 0xFF60) {
			return fmt.Errorf("replace %q must be printable single-cell characters", s)
		}
	}
	if strings.TrimSpace(s) == "" {
		return errors.New("replace must not be blank (it would hide that anything was masked)")
	}
	return nil
}

// Len is the number of rules.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.rules)
}

// Rule returns rule i as declared.
func (s *Set) Rule(i int) Rule { return s.rules[i].rule }

// Find returns the masked spans of one row's text, ascending and
// non-overlapping, and each rule's match count (the spans it contributed to,
// so a rule's overlapping patterns count once). Overlapping matches merge
// into one span; a merged span of rules with different replacements fills
// with Mask so nothing matched is ever left visible.
func (s *Set) Find(text string) ([]Span, []int) {
	if s == nil || len(s.rules) == 0 {
		return nil, nil
	}
	counts := make([]int, len(s.rules))
	var raw []Span
	var offsets []int // byte offset -> rune offset, built on the first match
	runeAt := func(b int) int {
		if offsets == nil {
			offsets = make([]int, len(text)+1)
			n := 0
			for i := 0; i < len(text); n++ {
				_, w := utf8.DecodeRuneInString(text[i:])
				for k := 0; k < w; k++ {
					offsets[i+k] = n
				}
				i += w
			}
			offsets[len(text)] = n
		}
		return offsets[b]
	}
	for i, c := range s.rules {
		for _, m := range c.matchers {
			for _, loc := range m.re.FindAllStringSubmatchIndex(text, -1) {
				a, b := loc[0], loc[1]
				if m.group > 0 {
					a, b = loc[2*m.group], loc[2*m.group+1]
				}
				// A checked preset value glued to a closing brace
				// ({password: x}, Config{Password: cfg.Password}) is
				// checked and masked without it. Pattern rules (no
				// check) keep their exact match.
				if m.check != nil {
					for b > a && text[b-1] == '}' {
						b--
					}
				}
				if a < 0 || b <= a {
					continue
				}
				if m.check != nil && !m.check(text[a:b]) {
					continue
				}
				raw = append(raw, Span{Start: runeAt(a), End: runeAt(b), Replace: c.replace, Rules: []int{i}})
			}
		}
	}
	if len(raw) == 0 {
		return nil, counts
	}
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].Start < raw[j].Start })
	out := []Span{raw[0]}
	for _, sp := range raw[1:] {
		last := &out[len(out)-1]
		if sp.Start >= last.End {
			out = append(out, sp)
			continue
		}
		last.End = max(last.End, sp.End)
		if sp.Replace != last.Replace {
			last.Replace = Mask
		}
		for _, r := range sp.Rules {
			if !slices.Contains(last.Rules, r) {
				last.Rules = append(last.Rules, r)
			}
		}
	}
	for _, sp := range out {
		for _, r := range sp.Rules {
			counts[r]++
		}
	}
	return out, counts
}

// ApplyText returns text with spans applied the way the cells are masked
// (fill per character, or the replacement text), for reports: examples show
// the masked row, never the secret.
func ApplyText(text string, spans []Span) string {
	runes := []rune(text)
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.Start < pos || sp.End > len(runes) {
			continue
		}
		b.WriteString(string(runes[pos:sp.Start]))
		if sp.Fill() {
			b.WriteString(strings.Repeat(sp.Replace, sp.End-sp.Start))
		} else {
			rep := []rune(sp.Replace)
			b.WriteString(string(rep[:min(len(rep), sp.End-sp.Start)]))
		}
		pos = sp.End
	}
	b.WriteString(string(runes[pos:]))
	return b.String()
}

// ---- presets ----

func presetReplace(name string) string {
	if name == PresetHomepaths {
		return "~"
	}
	return Mask
}

func must(expr string, check func(string) bool) matcher {
	re := regexp.MustCompile(expr)
	return matcher{re: re, group: re.SubexpIndex(SecretGroup), check: check}
}

// Secrets preset building blocks. A keyword is a whole word of the key: it
// starts the key or follows a non-letter (ENCRYPTION_KEY, x-api-key,
// "token"), or starts a camelCase word (accessToken, dbPassword); only
// digits, "_", "." or "-" may follow it before an optional quote and the
// ":", "=" or Go ":=" (TOKEN_1=), so keyboard, hotkey, monkey, tokenizer
// and password_policy are not keys. Credential keywords need a
// high-entropy value; password keywords any real value of 6+ characters.
// A bare "pass" key has matchers of its own below.
const (
	secretKeywords   = `api[_-]?key|access[_-]?key|secret[_-]?access[_-]?key|private[_-]?key|client[_-]?secret|secret|token|credentials?|key`
	secretCamel      = `[a-z](?:Api[Kk]ey|AccessKey|SecretKey|PrivateKey|ClientSecret|Secret|Token|Credentials?)`
	secretPascal     = `(?:^|[^A-Za-z])(?:Secret|Signing|Encryption)Key`
	passwordKeywords = `passw(?:or)?d|passphrase|pwd`
	passwordCamel    = `[a-z](?:Pass(?:w(?:or)?d|phrase)?|Pwd)`
	keySeparator     = `[0-9_.-]*["']?\s*(?::=|[:=])\s*`
	secretValue      = `["']?(?P<secret>[^\s"',;]{12,})`
	passwordValue    = `["']?(?P<secret>[^\s"',;]{6,})`
)

var presetMatchers = map[string][]matcher{
	PresetSecrets: {
		// OpenAI / Anthropic style keys: sk-…, sk-proj-…, sk-ant-api03-….
		must(`\bsk-[A-Za-z0-9_-]{20,}`, nil),
		// Stripe-style live/test keys.
		must(`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`, nil),
		// GitHub and GitLab tokens.
		must(`\bgh[pousr]_[A-Za-z0-9]{36,}`, nil),
		must(`\bgithub_pat_[A-Za-z0-9_]{22,}`, nil),
		must(`\bglpat-[A-Za-z0-9_-]{20,}`, nil),
		// AWS access key IDs.
		must(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, nil),
		// Slack tokens (xoxb-…, xapp-1-…).
		must(`\bxox[abposre]-[A-Za-z0-9-]{10,}`, nil),
		must(`\bxapp-[0-9]+-[A-Za-z0-9-]{10,}`, nil),
		// Google API keys, npm and PyPI tokens.
		must(`\bAIza[0-9A-Za-z_-]{35}`, nil),
		must(`\bnpm_[A-Za-z0-9]{36}`, nil),
		must(`\bpypi-[A-Za-z0-9_-]{32,}`, nil),
		// JSON Web Tokens.
		must(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`, nil),
		// Authorization: Bearer <token> / Basic <base64 user:password>.
		must(`(?i)\bbearer\s+(?P<secret>[A-Za-z0-9._~+/-]{16,}=*)`, nil),
		must(`(?i)\bbasic\s+(?P<secret>[A-Za-z0-9+/]{8,}={0,2})`, base64Credential),
		// Credentials in URLs: scheme://user:password@host.
		must(`://[^/\s:@]*:(?P<secret>[^@\s/]{4,})@`, nil),
		// PEM block bodies, line by line: a row that is one long base64 run,
		// or a block's short last line ending in = padding (AwEAAQ==).
		must(`^\s*(?P<secret>[A-Za-z0-9+/]{40,}={0,2})\s*$`, mixedBase64),
		must(`^\s*(?P<secret>[A-Za-z0-9+/]{2,38}={1,2})\s*$`, pemTail),
		// key=value / key: value and --flag VALUE / --flag=VALUE: a
		// high-entropy credential value, or any real password.
		must(`(?i)(?:^|[^A-Za-z])(?:`+secretKeywords+`)`+keySeparator+secretValue, highEntropy),
		must(secretCamel+keySeparator+secretValue, highEntropy),
		must(secretPascal+keySeparator+secretValue, highEntropy),
		must(`(?i)(?:^|\s)--(?:[a-z0-9]+-)*(?:api-?key|access-key|secret|token|key)(?:=|\s+)`+secretValue, highEntropy),
		must(`(?i)(?:^|[^A-Za-z])(?:`+passwordKeywords+`)`+keySeparator+passwordValue, passwordLike),
		must(passwordCamel+keySeparator+passwordValue, passwordLike),
		// A bare "pass" key is a final segment (SMTP_PASS:, db-pass=) or
		// takes "=" (pass=…), never "PASS:" (go test output) or bypass.
		must(`(?i)[_-]pass`+keySeparator+passwordValue, passwordLike),
		must(`(?i)(?:^|[^A-Za-z])pass[0-9_.-]*["']?\s*:?=\s*`+passwordValue, passwordLike),
		must(`(?i)(?:^|\s)--(?:[a-z0-9]+-)*(?:pass(?:w(?:or)?d|phrase)?|pwd)(?:=|\s+)`+passwordValue, passwordLike),
		// Command-line credentials: docker login -p X, mysql -pX (no space),
		// curl -u user:password; a quote before the value is skipped so
		// "$TOKEN" is seen as the placeholder it is.
		must(`\bdocker\s+login\b.*\s-p\s*["']?(?P<secret>[^\s"']{6,})`, notPlaceholder),
		must(`\bmysql\b.*\s-p["']?(?P<secret>[^\s"']{6,})`, notPlaceholder),
		must(`\bcurl\b.*\s(?:-u|--user)\s+[^\s:]+:["']?(?P<secret>[^\s"']{4,})`, notPlaceholder),
		// Authorization: Token <token> (needs a digit) and cookie values.
		must(`(?i)\bauthorization:\s*token\s+(?P<secret>[A-Za-z0-9._~+/-]{16,}=*)`, hasDigit),
		// The first and the last long pair of a cookie list are examined.
		must(`(?i)\b(?:set-)?cookie:\s*(?:[^=;\s]+=[^;\s]*;\s*)*?[^=;\s]+=(?P<secret>[^;\s]{16,})`, highEntropy),
		must(`(?i)\b(?:set-)?cookie:\s*(?:[^=;\s]+=[^;\s]*;\s*)*[^=;\s]+=(?P<secret>[^;\s]{16,})`, highEntropy),
	},
	PresetEmails: {
		must(`\b(?P<secret>[A-Za-z0-9._%+-]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*\.[A-Za-z]{2,})\b`, notSSHRemote),
	},
	PresetHomepaths: {
		// /home/<user> and /Users/<user>, not inside a longer path segment
		// (/var/home/…, x/home/…) and not /home itself.
		must(`(?:^|[^A-Za-z0-9._-])(?P<secret>/(?:home|Users)/[A-Za-z0-9._-]+)`, nil),
	},
}

// mixedBase64 accepts a base64 run that mixes upper case, lower case and
// digits (a key body), not a hex digest or a row of one letter.
func mixedBase64(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// highEntropy accepts a key=value value that looks like a credential: not a
// placeholder or variable reference, at least two character classes and at
// least 3 bits of Shannon entropy per character.
func highEntropy(s string) bool {
	if !notPlaceholder(s) || identifierLike(s) || codeValue(s) {
		return false
	}
	classes := 0
	var upper, lower, digit, other bool
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			other = true
		}
	}
	for _, b := range []bool{upper, lower, digit, other} {
		if b {
			classes++
		}
	}
	if classes < 2 {
		return false
	}
	h := 0.0
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h >= 3.0
}

// notPlaceholder rejects values that are not secrets themselves: variable
// references and templates ($VAR, ${VAR}, <token>, {{x}}, %s, $(cmd)), masks
// (****), calls and indexing (input("…"), os.environ["X"]), composite
// and pointer literals (&rsa.PrivateKey{}, Config{…}), hex constants of
// up to 16 digits (0x1F2E…), dotted references (process.env.TOKEN,
// settings.api_key), filesystem paths (/x, ./x, ../x, ~/x,
// certs/client-key.pem), URLs (a://…) and the literal documentation
// placeholders pass, password, secret and changeme.
func notPlaceholder(s string) bool {
	if s == "" || docPlaceholders[strings.ToLower(s)] || strings.ContainsAny(s[:1], "$<{%(*&") || strings.Contains(s, "://") || strings.ContainsAny(s, "([{}") || hexLiteral.MatchString(s) {
		return false
	}
	for _, p := range []string{"/", "./", "../", "~/"} {
		if strings.HasPrefix(s, p) {
			return false
		}
	}
	if dottedReference.MatchString(s) || (strings.Contains(s, "/") && fileExtension.MatchString(s)) {
		return false
	}
	return true
}

var (
	dottedReference = regexp.MustCompile(`^[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)+$`)
	fileExtension   = regexp.MustCompile(`\.[A-Za-z0-9]{1,5}$`)
	docPlaceholders = map[string]bool{"pass": true, "password": true, "secret": true, "changeme": true}
	// camelIdentifier is a pure-letter camelCase or PascalCase name
	// (hashedPassword, ContextKeyUser); codeValue also requires a
	// lower-case letter, so an all-caps word (HUNTERTWO) is a password.
	camelIdentifier = regexp.MustCompile(`^[A-Za-z][a-z]*(?:[A-Z][a-z]*)+$`)
	// hexLiteral is a short numeric constant (0x1F2E3D4C5B6A7988), not a
	// 0x-prefixed private key.
	hexLiteral = regexp.MustCompile(`^0x[0-9A-Fa-f]{1,16}$`)
	// wordIdentifier is an identifier-shaped value, the only shape
	// valueKeyword is applied to (MyPassword1! is a password).
	wordIdentifier = regexp.MustCompile(`^[A-Za-z_]\w*$`)
	// valueKeyword is a credential word inside a value (userPassword,
	// refreshToken, bcryptHash): a variable naming the secret. "hash"
	// counts only as a word of a compound name, so the plain word "hashed"
	// is a possible password and stays masked.
	valueKeyword = regexp.MustCompile(`(?i:passw(?:or)?d|token|secret)|Hash|HASH|hash[A-Z_]`)
	// keyChord is a keybinding that starts with a modifier (ctrl+shift+p,
	// Cmd+K), not base64 with a "+" in it.
	keyChord = regexp.MustCompile(`(?i)^(?:ctrl|control|shift|alt|opt|option|cmd|command|meta|super|win|mod|hyper|fn)(?:\+[A-Za-z0-9]+)+$`)
	// genericType is a parameterised type (Array<string>).
	genericType = regexp.MustCompile(`<[^>]*>`)
	// typeNames are declarations, not values (password: string).
	typeNames = map[string]bool{"string": true, "str": true, "bool": true, "boolean": true, "number": true, "int": true, "integer": true,
		"text": true, "bytes": true, "secretstr": true, "varchar": true, "char": true, "null": true, "none": true, "nil": true,
		"undefined": true, "optional": true, "required": true, "true": true, "false": true, "hidden": true}
)

// passwordLike accepts a password value: a real value, not a placeholder,
// call, reference, type name or code identifier. A lower-case word
// (password: hashed) cannot be told from a password and is masked.
func passwordLike(s string) bool {
	return notPlaceholder(s) && !typeNames[strings.ToLower(strings.TrimRight(s, "?!;:,)]}"))] && !codeValue(s)
}

// codeValue reports a value that is code, not a secret: a camelCase or
// PascalCase identifier, an identifier containing a credential word, a
// keybinding chord or a generic type.
func codeValue(s string) bool {
	return (camelIdentifier.MatchString(s) && strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz")) || (wordIdentifier.MatchString(s) && valueKeyword.MatchString(s)) || keyChord.MatchString(s) || genericType.MatchString(s)
}

// hasDigit accepts a token containing a digit.
func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// identifierLike rejects lower-case identifiers and key prefixes
// (sort_key=created_at_desc, "user:profile:").
func identifierLike(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && r != '_' && r != '-' && r != '.' && r != ':' {
			return false
		}
	}
	return true
}

// base64Credential accepts a Basic credential: base64 with a digit or
// padding, not an English word ("Basic authentication").
func base64Credential(s string) bool {
	return strings.ContainsAny(s, "0123456789=+/")
}

// pemTail accepts a short padded base64 row of whole quads mixing cases.
func pemTail(s string) bool {
	var upper, lower bool
	for _, r := range s {
		upper = upper || (r >= 'A' && r <= 'Z')
		lower = lower || (r >= 'a' && r <= 'z')
	}
	return len(s)%4 == 0 && upper && lower
}

// notSSHRemote leaves git@host SSH remotes alone.
func notSSHRemote(s string) bool { return !strings.HasPrefix(s, "git@") }
