//go:build unix

package run

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/mask"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/schedule"
)

const (
	defaultTailBytes = 4096
	maxTailBytes     = 8192
	maxSeconds       = 10 * 365 * 24 * 3600

	outputFailure = "failure"
	outputAlways  = "always"
	outputNever   = "never"

	usageText = `usage: cronbeat run --name JOB [--schedule "CRON"] [--timezone IANA] [--grace 5m|300] [--max-runtime 1h|3600]
                    [--output failure|always|never] [--tail-bytes N] [--api-url URL] [--key-file FILE] [--verbose] -- COMMAND [ARGS...]
`
)

var jobName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var maskText = mask.Text

type problem struct{ kind, msg string }

type options struct {
	name, schedule, timezone, grace, maxRuntime string
	output, apiURL, keyFile, tailBytesText      string
	verbose                                     bool
	tailBytes                                   int
}

type prepared struct {
	exit      int
	opts      options
	command   []string
	problems  []problem
	report    bool
	apiURL    string
	key       config.Secret
	keySource string
	job       client.Job
}

func commandOf(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func prepare(env Env) (p prepared) {
	p.exit = -1
	defer func() {
		if recover() != nil {
			p.report = false
			p.problems = append(p.problems, problem{"internal", "internal error while preparing the report; the command runs without monitoring"})
		}
	}()

	flagArgs, command, separated := splitArgs(env.Args)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := &p.opts
	fs.StringVar(&o.name, "name", "", "")
	fs.StringVar(&o.schedule, "schedule", "", "")
	fs.StringVar(&o.timezone, "timezone", "", "")
	fs.StringVar(&o.grace, "grace", "", "")
	fs.StringVar(&o.maxRuntime, "max-runtime", "", "")
	fs.StringVar(&o.output, "output", outputFailure, "")
	fs.StringVar(&o.tailBytesText, "tail-bytes", "", "")
	fs.StringVar(&o.apiURL, "api-url", "", "")
	fs.StringVar(&o.keyFile, "key-file", "", "")
	fs.BoolVar(&o.verbose, "verbose", false, "")
	err := fs.Parse(flagArgs)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(env.Stdout, usageText)
		p.exit = 0
		return p
	}
	p.command = command
	flagsOK := err == nil
	if !separated {
		p.command = fs.Args()
	} else if flagsOK && fs.NArg() > 0 {
		p.problems = append(p.problems, problem{"usage", fmt.Sprintf("ignoring unexpected argument %q before --", fs.Arg(0))})
	}
	if len(p.command) == 0 {
		msg := "missing command: cronbeat run --name JOB -- COMMAND [ARGS...]"
		if err != nil {
			msg = err.Error()
		}
		_, _ = fmt.Fprintf(env.Stderr, "cronbeat run: %s\n%s", msg, usageText)
		p.exit = exitUsage
		return p
	}
	if !flagsOK {
		p.problems = append(p.problems, problem{"usage", fmt.Sprintf("%v; the command runs without monitoring", err)})
		return p
	}

	p.resolveOutput()
	p.resolveJob(env)
	p.resolveConnection(env)
	return p
}

func splitArgs(args []string) (flagArgs, command []string, separated bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

func (p *prepared) problem(kind, format string, args ...any) {
	p.problems = append(p.problems, problem{kind, fmt.Sprintf(format, args...)})
}

func (p *prepared) resolveOutput() {
	o := &p.opts
	switch o.output {
	case outputFailure, outputAlways, outputNever:
	default:
		p.problem("usage", "--output must be failure, always or never, using %q", outputFailure)
		o.output = outputFailure
	}
	o.tailBytes = defaultTailBytes
	if o.tailBytesText == "" {
		return
	}
	n, err := strconv.Atoi(o.tailBytesText)
	switch {
	case err != nil || n < 1:
		p.problem("usage", "--tail-bytes must be a number between 1 and %d, using %d", maxTailBytes, defaultTailBytes)
	case n > maxTailBytes:
		p.problem("usage", "--tail-bytes is limited to %d", maxTailBytes)
		o.tailBytes = maxTailBytes
	default:
		o.tailBytes = n
	}
}

func (p *prepared) resolveJob(env Env) {
	o := &p.opts
	if !jobName.MatchString(o.name) {
		if o.name == "" {
			p.problem("usage", "--name is required to monitor a job (letters, digits, . _ -, up to 64); the command runs without monitoring")
		} else {
			p.problem("usage", "--name must use only letters, digits, . _ - and be at most 64 characters; the command runs without monitoring")
		}
		return
	}
	p.job.Name = o.name
	if o.schedule != "" {
		if expr, err := schedule.Normalize(o.schedule); err != nil {
			p.problem("usage", "--schedule ignored: %v", err)
		} else {
			p.job.Schedule = expr
		}
	}
	p.job.TimeZone = strings.TrimSpace(o.timezone)
	if p.job.TimeZone == "" && p.job.Schedule != "" && env.TimeZone != nil {
		p.job.TimeZone = env.TimeZone()
	}
	p.job.GraceSeconds = p.seconds("--grace", o.grace)
	p.job.MaxRuntimeSeconds = p.seconds("--max-runtime", o.maxRuntime)
}

func (p *prepared) seconds(flagName, text string) *int {
	if text == "" {
		return nil
	}
	n, err := parseSeconds(text)
	if err != nil {
		p.problem("usage", "%s ignored: %v", flagName, err)
		return nil
	}
	return &n
}

func parseSeconds(text string) (int, error) {
	text = strings.TrimSpace(text)
	if n, err := strconv.ParseUint(text, 10, 64); err == nil {
		if n < 1 || n > maxSeconds {
			return 0, errors.New("must be between 1 second and 10 years")
		}
		return int(n), nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 || d > maxSeconds*time.Second {
		return 0, fmt.Errorf("%q is not a number of seconds or a duration such as 5m", text)
	}
	return int((d + time.Second - 1) / time.Second), nil
}

func (p *prepared) resolveConnection(env Env) {
	if p.job.Name == "" {
		return
	}
	apiURL, err := config.APIURL(p.opts.apiURL, env.Getenv)
	if err != nil {
		p.problem("config", "%v; the command runs without monitoring", err)
		return
	}
	home := ""
	if env.Home != "" {
		home = filepath.Join(env.Home, ".config", "cronbeat", "key")
	}
	res := config.ResolveKey(config.KeySources{Getenv: env.Getenv, FlagFile: p.opts.keyFile, EtcPath: env.EtcKeyPath, HomePath: home})
	for _, rejected := range res.Rejected {
		p.problem("key-file", "%v", rejected)
	}
	if res.Key == "" {
		p.problem("no-key", "no API key configured, the command runs without monitoring (run `cronbeat configure`)")
		return
	}
	p.apiURL, p.key, p.keySource = apiURL, res.Key, res.Source
	p.report = true
}
