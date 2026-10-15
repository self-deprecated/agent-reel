package reel

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// usageError is a command-line mistake: an unknown command or flag, a missing
// or extra argument, or an invalid flag value. It exits 2 like a refused
// configuration.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }
func usageErr(format string, args ...any) error {
	return &usageError{fmt.Errorf(format, args...)}
}

// cliCommand is the curated help of one command.
type cliCommand struct {
	Name     string   // "export", "activity send", "service ui"
	Args     string   // "REC [OUT]"
	Summary  string   // one or more lines of description
	Examples []string // shown verbatim after "  "
	Docs     string   // docs file pointer
}

// cliFlags wraps flag.FlagSet: GNU-style --long help, curated per-command help
// on -h/--help, flags after positionals, and usage errors that exit 2.
type cliFlags struct {
	fs       *flag.FlagSet
	cmd      cliCommand
	out      io.Writer
	settings map[string]string // flag name -> setting key
	values   map[string]string // metavariable per flag name
	jsonOut  *bool
}

func newCLIFlags(cmd cliCommand, out io.Writer) *cliFlags {
	fs := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &cliFlags{fs: fs, cmd: cmd, out: out, settings: map[string]string{}, values: map[string]string{}}
}

// setting registers a string flag that overrides the setting key when given.
func (f *cliFlags) setting(name, key, meta, usage string) {
	f.fs.String(name, "", usage)
	f.settings[name] = key
	f.values[name] = meta
}

// settingBool registers a boolean flag that sets the setting key to true.
func (f *cliFlags) settingBool(name, key, usage string) {
	f.fs.Bool(name, false, usage)
	f.settings[name] = key
}

func (f *cliFlags) library() {
	f.setting("library", "library", "PATH", "recording library (default: config library)")
}
func (f *cliFlags) json() *bool {
	f.jsonOut = f.fs.Bool("json", false, "print machine-readable JSON")
	return f.jsonOut
}
func (f *cliFlags) String(name, meta, value, usage string) *string {
	f.values[name] = meta
	return f.fs.String(name, value, usage)
}
func (f *cliFlags) Int(name, meta string, value int, usage string) *int {
	f.values[name] = meta
	return f.fs.Int(name, value, usage)
}
func (f *cliFlags) Int64Var(p *int64, name, meta string, value int64, usage string) {
	f.values[name] = meta
	f.fs.Int64Var(p, name, value, usage)
}
func (f *cliFlags) IntVar(p *int, name, meta string, value int, usage string) {
	f.values[name] = meta
	f.fs.IntVar(p, name, value, usage)
}

// Strings registers a repeatable string flag; each occurrence appends.
func (f *cliFlags) Strings(name, meta, usage string) *[]string {
	f.values[name] = meta
	var v cliStrings
	f.fs.Var(&v, name, usage)
	return (*[]string)(&v)
}

type cliStrings []string

func (s *cliStrings) String() string     { return strings.Join(*s, ",") }
func (s *cliStrings) Set(v string) error { *s = append(*s, v); return nil }

func (f *cliFlags) Bool(name string, value bool, usage string) *bool {
	return f.fs.Bool(name, value, usage)
}

// parse parses flags anywhere among positionals. help reports -h/--help (the
// help was printed). Positional counts outside min..max are usage errors;
// max < 0 means unbounded.
func (f *cliFlags) parse(args []string, min, max int) (pos []string, help bool, err error) {
	rest := args
	for {
		if e := f.fs.Parse(rest); e != nil {
			if errors.Is(e, flag.ErrHelp) {
				return nil, true, f.printHelp()
			}
			return nil, false, f.flagError(e)
		}
		remaining := f.fs.Args()
		if len(remaining) == 0 {
			break
		}
		consumed := len(rest) - len(remaining)
		if consumed > 0 && rest[consumed-1] == "--" {
			pos = append(pos, remaining...)
			break
		}
		pos = append(pos, remaining[0])
		rest = remaining[1:]
	}
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		want := f.cmd.Args
		if want == "" {
			return nil, false, usageErr("%s: takes no arguments (got %q); see areel %s --help", f.cmd.Name, strings.Join(pos, " "), f.cmd.Name)
		}
		if len(pos) < min {
			return nil, false, usageErr("%s: missing argument; usage: areel %s %s", f.cmd.Name, f.cmd.Name, want)
		}
		return nil, false, usageErr("%s: too many arguments (got %q); usage: areel %s %s", f.cmd.Name, strings.Join(pos, " "), f.cmd.Name, want)
	}
	return pos, false, nil
}

func (f *cliFlags) flagError(e error) error {
	msg := e.Error()
	switch {
	case strings.HasPrefix(msg, "flag provided but not defined: -"):
		name := strings.TrimLeft(strings.TrimPrefix(msg, "flag provided but not defined: "), "-")
		var names []string
		f.fs.VisitAll(func(fl *flag.Flag) { names = append(names, fl.Name) })
		hint := ""
		if s := closest(name, names); s != "" {
			hint = fmt.Sprintf("; did you mean --%s?", s)
		}
		return usageErr("%s: unknown flag --%s%s (see areel %s --help)", f.cmd.Name, name, hint, f.cmd.Name)
	case strings.HasPrefix(msg, "flag needs an argument: -"):
		name := strings.TrimLeft(strings.TrimPrefix(msg, "flag needs an argument: "), "-")
		return usageErr("%s: --%s needs a value", f.cmd.Name, name)
	case strings.HasPrefix(msg, "invalid value "):
		// invalid value "x" for flag -fps: parse error
		msg = strings.Replace(msg, " for flag -", " for --", 1)
		return usageErr("%s: %s", f.cmd.Name, msg)
	case strings.HasPrefix(msg, "invalid boolean value "):
		msg = strings.Replace(msg, " for -", " for --", 1)
		return usageErr("%s: %s", f.cmd.Name, msg)
	}
	return usageErr("%s: %s", f.cmd.Name, msg)
}

// overrides returns the setting values given as flags on this invocation.
func (f *cliFlags) overrides() map[string]string {
	o := map[string]string{}
	f.fs.Visit(func(fl *flag.Flag) {
		if key, ok := f.settings[fl.Name]; ok {
			o[key] = fl.Value.String()
		}
	})
	return o
}

// isSet reports whether the flag was given on this invocation.
func (f *cliFlags) isSet(name string) bool {
	set := false
	f.fs.Visit(func(fl *flag.Flag) { set = set || fl.Name == name })
	return set
}

// load resolves the settings with this invocation's flag overrides.
func (f *cliFlags) load(extra map[string]string) (*settings, error) {
	o := f.overrides()
	for k, v := range extra {
		o[k] = v
	}
	return loadSettings(o, nil)
}

func (f *cliFlags) printHelp() error {
	_, e := io.WriteString(f.out, f.help())
	return e
}

func (f *cliFlags) help() string {
	var b strings.Builder
	usage := "areel " + f.cmd.Name
	if f.cmd.Args != "" {
		usage += " " + f.cmd.Args
	}
	fmt.Fprintf(&b, "Usage: %s [flags]\n\n", usage)
	for _, line := range strings.Split(strings.TrimSpace(f.cmd.Summary), "\n") {
		fmt.Fprintf(&b, "%s\n", line)
	}
	type row struct{ left, right string }
	var rows []row
	width := 0
	f.fs.VisitAll(func(fl *flag.Flag) {
		left := "--" + fl.Name
		meta, _ := flag.UnquoteUsage(fl)
		if m, ok := f.values[fl.Name]; ok {
			meta = m
		}
		if isBoolFlag(fl) {
			meta = ""
		}
		if meta != "" {
			left += " " + meta
		}
		right := fl.Usage
		if _, isSetting := f.settings[fl.Name]; !isSetting && fl.DefValue != "" && fl.DefValue != "0" && fl.DefValue != "false" {
			right += fmt.Sprintf(" (default %s)", fl.DefValue)
		}
		rows = append(rows, row{left, right})
		width = max(width, len(left))
	})
	rows = append(rows, row{"-h, --help", "show this help"})
	width = max(width, len("-h, --help"))
	b.WriteString("\nFlags:\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, r.left, r.right)
	}
	if len(f.cmd.Examples) > 0 {
		b.WriteString("\nExamples:\n")
		for _, ex := range f.cmd.Examples {
			fmt.Fprintf(&b, "  %s\n", ex)
		}
	}
	if f.cmd.Docs != "" {
		fmt.Fprintf(&b, "\nDocs: %s\n", f.cmd.Docs)
	}
	return b.String()
}

func isBoolFlag(fl *flag.Flag) bool {
	b, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// closest returns the candidate within a small edit distance of word, or "".
func closest(word string, candidates []string) string {
	best, bestD := "", 1<<30
	for _, c := range candidates {
		d := editDistance(word, c)
		if strings.HasPrefix(c, word) && len(word) >= 2 {
			d = min(d, 1)
		}
		if d < bestD {
			best, bestD = c, d
		}
	}
	if bestD <= max(2, len(word)/3) {
		return best
	}
	return ""
}

func suggestSuffix(word string, candidates []string) string {
	if s := closest(word, candidates); s != "" {
		return fmt.Sprintf("; did you mean %q?", s)
	}
	return ""
}

func editDistance(a, b string) int {
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
