//go:build unix

package crontab

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const (
	exitFailure = 1
	exitUsage   = 64
)

// Env is what the crontab command needs from its surroundings.
type Env struct {
	Args       []string
	Stdout     io.Writer
	Stderr     io.Writer
	Getenv     func(string) string
	Home       string
	Executable func() (string, error)
	CrontabBin string
	Now        func() time.Time
}

const keyHint = "The API key is never written to the crontab. Run `cronbeat configure` as the user that owns these jobs,\n" +
	"or add a line `CRONBEAT_API_KEY=<key>` at the top of the crontab.\n"

type settings struct {
	file  string
	cronD bool
	force bool
}

// Main runs "cronbeat crontab ..." and returns the process exit code.
func Main(env Env) int {
	if len(env.Args) == 0 {
		return usage(env, "missing subcommand")
	}
	sub := env.Args[0]
	if sub != "plan" && sub != "apply" && sub != "undo" {
		return usage(env, fmt.Sprintf("unknown subcommand %q", sub))
	}
	var s settings
	fs := flag.NewFlagSet("crontab "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&s.file, "file", "", "")
	fs.BoolVar(&s.cronD, "cron-d", false, "")
	if sub == "undo" {
		fs.BoolVar(&s.force, "force", false, "")
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return usage(env, err.Error())
	}
	if fs.NArg() > 0 {
		return usage(env, fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if err := checkFileFlags(s); err != nil {
		return usage(env, err.Error())
	}

	var store Store = UserStore{Bin: env.CrontabBin}
	label := "your crontab"
	if s.file != "" {
		store = FileStore{Path: s.file}
		label = store.Target()
	}
	switch sub {
	case "plan":
		return runPlan(env, store, label, s)
	case "apply":
		return runApply(env, store, label, s)
	default:
		return runUndo(env, store, label, s)
	}
}

func checkFileFlags(s settings) error {
	if s.cronD && s.file == "" {
		return errors.New("--cron-d needs --file, for example --file /etc/cron.d/myapp --cron-d")
	}
	if !s.cronD && s.file != "" {
		clean := filepath.Clean(s.file)
		if clean == "/etc/crontab" || strings.HasPrefix(clean, "/etc/cron.d/") {
			return errors.New("system crontabs have a user column, add --cron-d")
		}
	}
	return nil
}

func usage(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "cronbeat crontab: %s\nusage: cronbeat crontab plan|apply|undo [--file FILE] [--cron-d] [--force (undo)]\n", msg)
	return exitUsage
}

func fail(env Env, err error) int {
	_, _ = fmt.Fprintf(env.Stderr, "cronbeat crontab: %v\n", err)
	return exitFailure
}

func options(env Env, s settings) (Options, error) {
	exe, err := env.Executable()
	if err != nil {
		return Options{}, fmt.Errorf("cannot find the cronbeat binary: %w", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return Options{}, fmt.Errorf("cannot find the cronbeat binary: %w", err)
	}
	return Options{BinaryPath: abs, CronD: s.cronD}, nil
}

func backups(env Env) *Backups {
	base := env.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(env.Home, ".local", "state")
	}
	return &Backups{Dir: filepath.Join(base, "cronbeat", "crontab-backups"), Now: env.Now}
}

func runPlan(env Env, store Store, label string, s settings) int {
	opts, err := options(env, s)
	if err != nil {
		return fail(env, err)
	}
	current, err := store.Read()
	if err != nil {
		return fail(env, err)
	}
	plan, err := Build(current, opts)
	if err != nil {
		return fail(env, err)
	}
	_, _ = fmt.Fprintf(env.Stdout, "cronbeat crontab plan for %s\n%s", label, plan.Report())
	if plan.Changed() {
		_, _ = fmt.Fprintf(env.Stdout, "Nothing was written. Apply with: %s\n%s", command("apply", s), keyHint)
	}
	return 0
}

func runApply(env Env, store Store, label string, s settings) int {
	opts, err := options(env, s)
	if err != nil {
		return fail(env, err)
	}
	res, err := Apply(store, backups(env), opts)
	if err != nil {
		return fail(env, err)
	}
	_, _ = fmt.Fprintf(env.Stdout, "cronbeat crontab apply for %s\n%s", label, res.Plan.Report())
	if !res.Changed {
		_, _ = fmt.Fprintln(env.Stdout, "Nothing to change.")
		return 0
	}
	_, _ = fmt.Fprintf(env.Stdout, "Applied and verified.\nBackup: %s\nUndo with: %s\n%s", res.BackupPath, command("undo", s), keyHint)
	return 0
}

func runUndo(env Env, store Store, label string, s settings) int {
	res, err := Undo(store, backups(env), s.force)
	if err != nil {
		return fail(env, err)
	}
	_, _ = fmt.Fprintf(env.Stdout, "Restored %s from %s\n", label, res.BackupPath)
	return 0
}

func command(sub string, s settings) string {
	cmd := "cronbeat crontab " + sub
	if s.file != "" {
		cmd += " --file " + word(s.file)
	}
	if s.cronD {
		cmd += " --cron-d"
	}
	return cmd
}
