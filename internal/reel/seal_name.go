package reel

// This renderer intentionally depends only on State and the standard library,
// so other producers can use the same naming grammar.
import (
	"fmt"
	"regexp"
	"strings"
)

type sealNamePart struct {
	literal string
	token   string
}

func sealNameByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// parseSealName validates syntax and the minimum possible rendered length.
// The actual rendering is checked too, because target/backend lengths vary.
func parseSealName(tpl string) ([]sealNamePart, error) {
	if strings.HasPrefix(tpl, ".") {
		return nil, fmt.Errorf("seal name must not start with .")
	}
	if strings.Contains(tpl, "..") {
		return nil, fmt.Errorf("seal name must not contain ..")
	}
	var parts []sealNamePart
	ids, minimum := 0, 0
	for i := 0; i < len(tpl); {
		if tpl[i] == '{' {
			end := strings.IndexByte(tpl[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("seal name has an unterminated token")
			}
			token := tpl[i+1 : i+end]
			switch token {
			case "id":
				ids++
				minimum += 32
			case "date":
				minimum += 10
			case "time":
				minimum += 6
			case "backend", "target":
				minimum++
			default:
				return nil, fmt.Errorf("seal name has unknown token {%s}", token)
			}
			parts = append(parts, sealNamePart{token: token})
			i += end + 1
		} else {
			start := i
			for i < len(tpl) && tpl[i] != '{' {
				if !sealNameByte(tpl[i]) {
					return nil, fmt.Errorf("seal name literal bytes must be [A-Za-z0-9._-] (got %q)", tpl[i])
				}
				i++
			}
			parts = append(parts, sealNamePart{literal: tpl[start:i]})
			minimum += i - start
		}
	}
	if ids != 1 {
		return nil, fmt.Errorf("seal name requires exactly one {id} token (got %d)", ids)
	}
	if minimum > 200 {
		return nil, fmt.Errorf("seal name rendered length must be at most 200 bytes")
	}
	return parts, nil
}

func renderSealName(tpl string, state State) (string, error) {
	parts, err := parseSealName(tpl)
	if err != nil {
		return "", err
	}
	if !sealIDPattern.MatchString(state.ID) {
		return "", fmt.Errorf("seal name {id} must be 32 lowercase hexadecimal bytes")
	}
	at := state.created
	if len(state.Segments) > 0 && !state.Segments[0].Started.IsZero() {
		at = state.Segments[0].Started
	}
	var b strings.Builder
	for _, p := range parts {
		value := p.literal
		switch p.token {
		case "id":
			value = state.ID
		case "date", "time":
			if at.IsZero() {
				return "", fmt.Errorf("seal name {%s} requires first segment start or store created time", p.token)
			}
			layout := "2006-01-02"
			if p.token == "time" {
				layout = "150405"
			}
			value = at.UTC().Format(layout)
		case "backend":
			value = state.Target.Backend
			if len(value) < 1 || len(value) > 64 {
				return "", fmt.Errorf("seal name {backend} must be 1..64 bytes")
			}
			for i := range value {
				if !sealNameByte(value[i]) {
					return "", fmt.Errorf("seal name {backend} must use [A-Za-z0-9._-]")
				}
			}
		case "target":
			value = state.Target.Name
			if len(value) > 64 {
				value = value[:64]
			}
			mapped := []byte(value)
			for i, c := range mapped {
				if !sealNameByte(c) {
					mapped[i] = '_'
				}
			}
			value = string(mapped)
			if value == "" {
				value = "_"
			}
		}
		b.WriteString(value)
	}
	name := b.String()
	if len(name) > 200 {
		return "", fmt.Errorf("seal name rendered length must be at most 200 bytes (got %d)", len(name))
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("seal name must not start with .")
	}
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("seal name must not contain ..")
	}
	return name, nil
}

// The sole capture is the recording ID. Other tokens intentionally use the
// same broad alphabet so locating does not require opening visual metadata.
func sealNameRegexp(tpl string) (*regexp.Regexp, error) {
	parts, err := parseSealName(tpl)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteByte('^')
	for _, p := range parts {
		switch p.token {
		case "":
			b.WriteString(regexp.QuoteMeta(p.literal))
		case "id":
			b.WriteString(`([0-9a-f]{32})`)
		default:
			b.WriteString(`[A-Za-z0-9._-]{1,64}`)
		}
	}
	b.WriteString(`\.areel$`)
	return regexp.Compile(b.String())
}

var sealIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// A request owns its cache; no unbounded process-global template registry.
type sealNameCache map[string]*regexp.Regexp

func (c *sealNameCache) compile(tpl string) (*regexp.Regexp, error) {
	if tpl == "" {
		tpl = "{id}"
	}
	if re := (*c)[tpl]; re != nil {
		return re, nil
	}
	re, err := sealNameRegexp(tpl)
	if err != nil {
		return nil, err
	}
	if *c == nil {
		*c = make(sealNameCache)
	}
	(*c)[tpl] = re
	return re, nil
}
