// Package crontab wraps the jobs of a crontab with "cronbeat run" and can undo that.
package crontab

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/schedule"
)

const (
	maxLineBytes = 1000
	maxNameBytes = 64
	counterRoom  = 3
	hashLength   = 6
	defaultShell = "/bin/sh"
)

// Action tells what happens to one line.
type Action int

// Line actions.
const (
	Keep Action = iota
	Wrap
	Skip
)

// Options configures Build.
type Options struct {
	// BinaryPath is the absolute path of the cronbeat binary written into the crontab.
	BinaryPath string
	// CronD parses lines of /etc/crontab and /etc/cron.d files, which have a user column.
	CronD bool
}

// Line is the result for one input line.
type Line struct {
	Number    int
	Original  string
	Rewritten string
	Action    Action
	Reason    string
	Name      string
}

// Plan is the outcome of Build.
type Plan struct {
	Input     string
	Lines     []Line
	Output    string
	Wrapped   int
	Skipped   int
	Unchanged int
}

// Changed reports whether Output differs from the input.
func (p Plan) Changed() bool { return p.Output != p.Input }

var (
	envLine        = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)$`)
	alreadyWrapped = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])cronbeat['"]?[ \t]+run(?:[ \t]|$)`)
	startsWithTool = regexp.MustCompile(`^['"]?(?:[^ \t]*/)?cronbeat['"]?(?:[ \t]|$)`)
	nameFlag       = regexp.MustCompile(`--name[ =]['"]?([A-Za-z0-9._-]+)`)
	userName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*\$?$`)
	safeWord       = regexp.MustCompile(`^[A-Za-z0-9_./+:@=-]+$`)
	safeShell      = regexp.MustCompile(`^/[A-Za-z0-9_./+-]+$`)
	timeZone       = regexp.MustCompile(`^[A-Za-z0-9_+/-]{1,64}$`)
	envAssignment  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

var interpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "python": true, "python2": true,
	"python3": true, "perl": true, "ruby": true, "node": true, "php": true,
}

var scriptExtensions = []string{".sh", ".py", ".rb", ".pl", ".js", ".php"}

type job struct {
	prefix   string
	schedule string
	command  string
}

type candidate struct {
	index int
	job   job
	base  string
}

// Build wraps the jobs of content.
func Build(content string, opts Options) (Plan, error) {
	if err := validateBinary(opts.BinaryPath); err != nil {
		return Plan{}, err
	}
	pieces := splitLines(content)
	plan := Plan{Input: content, Lines: make([]Line, len(pieces))}
	env := environment{shell: defaultShell}
	used := map[string]bool{}
	var candidates []candidate
	states := make([]environment, len(pieces))

	for i, piece := range pieces {
		line := Line{Number: i + 1, Original: piece.body}
		states[i] = env
		switch kind, rest := classify(piece.body); kind {
		case kindBlank, kindComment:
			line.Action = Keep
		case kindEnv:
			line.Action = Keep
			env = env.apply(rest)
		default:
			j, reason := parseJob(piece.body, opts.CronD)
			switch {
			case reason != "":
				line.Action, line.Reason = Skip, reason
			default:
				line.Action = Wrap
				candidates = append(candidates, candidate{index: i, job: j, base: commandName(j.command)})
			}
		}
		if names := nameFlag.FindStringSubmatch(piece.body); names != nil && line.Action == Skip {
			used[names[1]] = true
		}
		plan.Lines[i] = line
	}

	for _, c := range candidates {
		name := uniqueName(c.base, c.job, used)
		used[name] = true
		line := &plan.Lines[c.index]
		rewritten := rewrite(c.job, name, opts.BinaryPath, states[c.index])
		if len(rewritten) > maxLineBytes {
			line.Action = Skip
			line.Reason = fmt.Sprintf("wrapped line would be %d bytes, cron may ignore lines over %d", len(rewritten), maxLineBytes)
			continue
		}
		line.Name, line.Rewritten = name, rewritten
	}

	var out strings.Builder
	for i, piece := range pieces {
		line := plan.Lines[i]
		switch line.Action {
		case Wrap:
			out.WriteString(line.Rewritten)
			plan.Wrapped++
		case Skip:
			out.WriteString(piece.body)
			plan.Skipped++
		default:
			out.WriteString(piece.body)
			plan.Unchanged++
		}
		out.WriteString(piece.eol)
	}
	plan.Output = out.String()
	return plan, nil
}

func validateBinary(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return errors.New("the cronbeat binary path must be absolute")
	case strings.ContainsAny(p, "%\n\r\x00"):
		return errors.New("the cronbeat binary path must not contain %, newlines or NUL")
	}
	return nil
}

type piece struct{ body, eol string }

func splitLines(content string) []piece {
	var pieces []piece
	for _, raw := range strings.SplitAfter(content, "\n") {
		if raw == "" {
			continue
		}
		body, eol := raw, ""
		if strings.HasSuffix(body, "\n") {
			body, eol = body[:len(body)-1], "\n"
		}
		if strings.HasSuffix(body, "\r") {
			body, eol = body[:len(body)-1], "\r"+eol
		}
		pieces = append(pieces, piece{body: body, eol: eol})
	}
	return pieces
}

type environment struct {
	shell string
	tz    string
}

func (e environment) apply(assignment string) environment {
	m := envLine.FindStringSubmatch(assignment)
	if m == nil {
		return e
	}
	value := strings.TrimSpace(m[2])
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	switch m[1] {
	case "SHELL":
		e.shell = defaultShell
		if safeShell.MatchString(value) {
			e.shell = value
		}
	case "CRON_TZ":
		e.tz = ""
		if timeZone.MatchString(value) {
			e.tz = value
		}
	}
	return e
}

type lineKind int

const (
	kindBlank lineKind = iota
	kindComment
	kindEnv
	kindJob
)

func classify(body string) (lineKind, string) {
	trimmed := strings.TrimSpace(body)
	switch {
	case trimmed == "":
		return kindBlank, ""
	case strings.HasPrefix(trimmed, "#"):
		return kindComment, ""
	case envLine.MatchString(body):
		return kindEnv, body
	default:
		return kindJob, ""
	}
}

func isBlank(b byte) bool { return b == ' ' || b == '\t' }

func nextToken(s string, from int) (start, end int) {
	start = from
	for start < len(s) && isBlank(s[start]) {
		start++
	}
	end = start
	for end < len(s) && !isBlank(s[end]) {
		end++
	}
	return start, end
}

const malformed = "malformed crontab line"

func parseJob(body string, cronD bool) (job, string) {
	start, end := nextToken(body, 0)
	if start == end {
		return job{}, malformed
	}
	first := body[start:end]
	var expr string
	cursor := end
	if strings.HasPrefix(first, "@") {
		if schedule.IsReboot(first) {
			return job{}, "@reboot has no schedule"
		}
		macro, ok := schedule.Macro(first)
		if !ok {
			return job{}, malformed
		}
		expr = macro
	} else {
		fields := make([]string, 0, 5)
		cursor = 0
		for i := range 5 {
			s, e := nextToken(body, cursor)
			if s == e || !schedule.ValidField(i, body[s:e]) {
				return job{}, malformed
			}
			fields = append(fields, body[s:e])
			cursor = e
		}
		expr = strings.Join(fields, " ")
	}
	if cronD {
		s, e := nextToken(body, cursor)
		if s == e || !userName.MatchString(body[s:e]) {
			return job{}, malformed
		}
		cursor = e
	}
	commandStart, _ := nextToken(body, cursor)
	command := body[commandStart:]
	if strings.TrimSpace(command) == "" {
		return job{}, malformed
	}
	switch {
	case alreadyWrapped.MatchString(command), startsWithTool.MatchString(command):
		return job{}, "already uses cronbeat"
	case hasUnescapedPercent(command):
		return job{}, "unescaped % (cron turns it into a newline)"
	case strings.HasPrefix(command, "-"):
		return job{}, "command starts with -"
	}
	return job{prefix: body[:commandStart], schedule: expr, command: command}, ""
}

func hasUnescapedPercent(command string) bool {
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '\\':
			i++
		case '%':
			return true
		}
	}
	return false
}

func commandName(command string) string {
	tokens := strings.Fields(command)
	for {
		for len(tokens) > 0 && envAssignment.MatchString(tokens[0]) {
			tokens = tokens[1:]
		}
		if len(tokens) > 0 && tokens[0] == "cd" {
			if i := indexOf(tokens, "&&"); i >= 0 {
				tokens = tokens[i+1:]
				continue
			}
		}
		break
	}
	if len(tokens) > 0 && baseName(tokens[0]) == "env" {
		tokens = tokens[1:]
		for len(tokens) > 0 && (envAssignment.MatchString(tokens[0]) || strings.HasPrefix(tokens[0], "-")) {
			tokens = tokens[1:]
		}
	}
	if len(tokens) == 0 {
		return "job"
	}
	word := tokens[0]
	if interpreters[baseName(word)] {
		for _, arg := range tokens[1:] {
			if !strings.HasPrefix(arg, "-") {
				word = arg
				break
			}
		}
	}
	return sanitizeName(word)
}

func indexOf(tokens []string, want string) int {
	for i, t := range tokens {
		if t == want {
			return i
		}
	}
	return -1
}

func baseName(word string) string {
	return path.Base(strings.Trim(word, `'"`))
}

func sanitizeName(word string) string {
	name := baseName(word)
	for _, ext := range scriptExtensions {
		if trimmed, ok := strings.CutSuffix(name, ext); ok && trimmed != "" {
			name = trimmed
			break
		}
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name = strings.Trim(b.String(), "-.")
	if name == "" {
		return "job"
	}
	return strings.TrimRight(name[:min(len(name), maxNameBytes-hashLength-1-counterRoom)], "-.")
}

func uniqueName(base string, j job, used map[string]bool) string {
	if !used[base] {
		return base
	}
	sum := sha256.Sum256([]byte(j.schedule + "\x00" + j.command))
	name := base + "-" + hex.EncodeToString(sum[:])[:hashLength]
	for n := 2; used[name]; n++ {
		name = fmt.Sprintf("%s-%s-%d", base, hex.EncodeToString(sum[:])[:hashLength], n)
	}
	return name
}

func rewrite(j job, name, binary string, env environment) string {
	var b strings.Builder
	b.WriteString(j.prefix)
	b.WriteString(word(binary))
	b.WriteString(" run --name ")
	b.WriteString(name)
	b.WriteString(" --schedule ")
	b.WriteString(quote(j.schedule))
	if env.tz != "" {
		b.WriteString(" --timezone ")
		b.WriteString(quote(env.tz))
	}
	b.WriteString(" -- ")
	b.WriteString(word(env.shell))
	b.WriteString(" -c ")
	b.WriteString(quote(j.command))
	return b.String()
}

func word(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return quote(s)
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Report renders the plan as text.
func (p Plan) Report() string {
	var b strings.Builder
	for _, l := range p.Lines {
		switch l.Action {
		case Wrap:
			fmt.Fprintf(&b, "line %d: wrap as %q\n  - %s\n  + %s\n", l.Number, l.Name, l.Original, l.Rewritten)
		case Skip:
			fmt.Fprintf(&b, "line %d: skip, %s\n", l.Number, l.Reason)
		}
	}
	fmt.Fprintf(&b, "%d wrapped, %d skipped, %d unchanged.\n", p.Wrapped, p.Skipped, p.Unchanged)
	return b.String()
}
