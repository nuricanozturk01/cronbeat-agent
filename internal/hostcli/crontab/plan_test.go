package crontab

import (
	"fmt"
	"strings"
	"testing"
)

const bin = "/usr/local/bin/cronbeat"

func build(t *testing.T, content string, cronD bool) Plan {
	t.Helper()
	plan, err := Build(content, Options{BinaryPath: bin, CronD: cronD})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func wrapped(name, schedule, shell, command string) string {
	return fmt.Sprintf("%s run --name %s --schedule '%s' -- %s -c '%s'", bin, name, schedule, shell, command)
}

func TestBuildWrapsASimpleJob(t *testing.T) {
	plan := build(t, "0 2 * * * /opt/backup.sh\n", false)
	want := "0 2 * * * " + wrapped("backup", "0 2 * * *", "/bin/sh", "/opt/backup.sh") + "\n"
	if plan.Output != want {
		t.Fatalf("Output =\n%q\nwant\n%q", plan.Output, want)
	}
	if !plan.Changed() || plan.Wrapped != 1 || plan.Skipped != 0 || plan.Unchanged != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestCommentsBlankAndEnvironmentLinesAreUntouched(t *testing.T) {
	in := "# m h dom mon dow command\n\n   \n  # indented comment\nMAILTO=ops@example.com\nPATH=/usr/bin:/bin\nHOME = /root\n_X1=1\n#0 2 * * * /opt/commented-out.sh\n"
	plan := build(t, in, false)
	if plan.Output != in || plan.Changed() || plan.Unchanged != 9 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestAlreadyWrappedLinesAreSkipped(t *testing.T) {
	lines := []string{
		"0 2 * * * /usr/local/bin/cronbeat run --name x --schedule '0 2 * * *' -- /bin/sh -c 'a'",
		"0 2 * * * cronbeat run --name x -- a",
		"0 2 * * * cd /app && /opt/bin/cronbeat run --name y -- ./go.sh",
		"0 2 * * * '/opt/my tools/cronbeat' run --name z -- a",
		"0 2 * * * /usr/local/bin/cronbeat check",
	}
	for _, line := range lines {
		plan := build(t, line+"\n", false)
		if plan.Changed() || plan.Skipped != 1 || !strings.Contains(plan.Lines[0].Reason, "cronbeat") {
			t.Errorf("%q: plan = %+v", line, plan)
		}
	}
}

func TestSomethingMerelyNamedLikeCronbeatIsStillWrapped(t *testing.T) {
	plan := build(t, "0 2 * * * /opt/cronbeat-backup.sh\n0 3 * * * /opt/mycronbeat run\n", false)
	if plan.Wrapped != 2 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestRebootIsSkippedAndMacrosAreMapped(t *testing.T) {
	plan := build(t, "@reboot /opt/boot.sh\n", false)
	if plan.Changed() || plan.Skipped != 1 || !strings.Contains(plan.Lines[0].Reason, "@reboot") {
		t.Fatalf("plan = %+v", plan)
	}
	macros := map[string]string{
		"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@weekly": "0 0 * * 0",
		"@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@DAILY": "0 0 * * *",
	}
	for macro, expr := range macros {
		plan := build(t, macro+" /opt/job.sh\n", false)
		want := macro + " " + wrapped("job", expr, "/bin/sh", "/opt/job.sh") + "\n"
		if plan.Output != want {
			t.Errorf("%s: Output = %q, want %q", macro, plan.Output, want)
		}
	}
}

func TestUnescapedPercentIsSkippedEscapedPercentIsWrapped(t *testing.T) {
	skipped := []string{
		"0 3 * * * date +%F >> /var/log/d.log",
		"0 3 * * * /opt/x.sh %input",
		"0 3 * * * echo a\\\\%b",
	}
	for _, line := range skipped {
		plan := build(t, line+"\n", false)
		if plan.Changed() || plan.Skipped != 1 || !strings.Contains(plan.Lines[0].Reason, "%") {
			t.Errorf("%q: plan = %+v", line, plan)
		}
	}
	plan := build(t, "0 3 * * * date +\\%F >> /var/log/d.log\n", false)
	want := "0 3 * * * " + wrapped("date", "0 3 * * *", "/bin/sh", `date +\%F >> /var/log/d.log`) + "\n"
	if plan.Output != want {
		t.Fatalf("Output = %q, want %q", plan.Output, want)
	}
}

func TestSingleQuotesAreEscaped(t *testing.T) {
	plan := build(t, "* * * * * echo 'hi there' && echo \"it's\"\n", false)
	want := "* * * * * " + wrapped("echo", "* * * * *", "/bin/sh", `echo '\''hi there'\'' && echo "it'\''s"`) + "\n"
	if plan.Output != want {
		t.Fatalf("Output = %q\nwant      %q", plan.Output, want)
	}
}

func TestShellMetacharactersStayInsideTheSingleArgument(t *testing.T) {
	cmd := "cd /app && ./run.sh | tee -a /tmp/x 2>&1; echo $HOME `date` $(id) > /dev/null"
	plan := build(t, "*/5 * * * * "+cmd+"\n", false)
	want := "*/5 * * * * " + wrapped("run", "*/5 * * * *", "/bin/sh", cmd) + "\n"
	if plan.Output != want {
		t.Fatalf("Output = %q\nwant      %q", plan.Output, want)
	}
}

func TestOriginalWhitespaceIsPreservedAndScheduleIsNormalised(t *testing.T) {
	plan := build(t, "  0   2\t*  * *    /opt/a.sh  \n", false)
	want := "  0   2\t*  * *    " + wrapped("a", "0 2 * * *", "/bin/sh", "/opt/a.sh  ") + "\n"
	if plan.Output != want {
		t.Fatalf("Output = %q\nwant      %q", plan.Output, want)
	}
}

func TestNamesComeFromTheCommandAndAreStable(t *testing.T) {
	tests := []struct{ command, name string }{
		{"/opt/backup.sh --full", "backup"},
		{"/usr/bin/php /srv/app/artisan schedule:run", "artisan"},
		{"python3 -u /srv/jobs/sync_data.py --full", "sync_data"},
		{"/usr/bin/env bash /srv/jobs/rotate.sh", "rotate"},
		{"FOO=bar BAZ=1 /opt/x.sh", "x"},
		{"cd /app && ./nightly", "nightly"},
		{"cd /app", "cd"},
		{"/opt/weird$name!.sh", "weird-name"},
		{"/opt/my-tool_v1.2", "my-tool_v1.2"},
		{"çalış", "al"},
		{"日本語", "job"},
		{"./run.sh", "run"},
		{strings.Repeat("a", 100), strings.Repeat("a", 54)},
	}
	for _, tt := range tests {
		plan := build(t, "0 1 * * * "+tt.command+"\n", false)
		if len(plan.Lines) != 1 || plan.Lines[0].Name != tt.name {
			t.Errorf("%q: name = %q, want %q (%+v)", tt.command, plan.Lines[0].Name, tt.name, plan.Lines[0])
		}
	}
}

func TestCollidingNamesGetAStableHashSuffix(t *testing.T) {
	in := "0 1 * * * /opt/a/backup.sh\n0 2 * * * /opt/b/backup.sh\n0 3 * * * /opt/c/backup.sh\n"
	first := build(t, in, false)
	second := build(t, in, false)
	if first.Output != second.Output {
		t.Fatal("output is not deterministic")
	}
	names := []string{first.Lines[0].Name, first.Lines[1].Name, first.Lines[2].Name}
	if names[0] != "backup" {
		t.Fatalf("first occurrence keeps the plain name: %v", names)
	}
	for _, n := range names[1:] {
		if !strings.HasPrefix(n, "backup-") || len(n) != len("backup-")+6 {
			t.Fatalf("collision suffix: %v", names)
		}
	}
	if names[1] == names[2] {
		t.Fatalf("suffixes must differ: %v", names)
	}
	reordered := build(t, "0 3 * * * /opt/c/backup.sh\n0 2 * * * /opt/b/backup.sh\n", false)
	if reordered.Lines[1].Name != names[1] {
		t.Fatalf("hash suffix depends on position: %q vs %q", reordered.Lines[1].Name, names[1])
	}
}

func TestIdenticalLinesStillGetUniqueNames(t *testing.T) {
	plan := build(t, "0 1 * * * /opt/x.sh\n0 1 * * * /opt/x.sh\n0 1 * * * /opt/x.sh\n", false)
	seen := map[string]bool{}
	for _, l := range plan.Lines {
		if seen[l.Name] {
			t.Fatalf("duplicate name %q", l.Name)
		}
		seen[l.Name] = true
	}
}

func TestNamesOfAlreadyWrappedJobsAreNotReused(t *testing.T) {
	in := "0 1 * * * /usr/local/bin/cronbeat run --name backup -- /opt/old.sh\n0 2 * * * /opt/backup.sh\n"
	plan := build(t, in, false)
	if plan.Lines[1].Name == "backup" || !strings.HasPrefix(plan.Lines[1].Name, "backup-") {
		t.Fatalf("name = %q", plan.Lines[1].Name)
	}
}

func TestCronDKeepsTheUserColumn(t *testing.T) {
	plan := build(t, "MAILTO=root\n30 4 * * 1  www-data  /opt/clean.sh --all\n", true)
	want := "MAILTO=root\n30 4 * * 1  www-data  " + wrapped("clean", "30 4 * * 1", "/bin/sh", "/opt/clean.sh --all") + "\n"
	if plan.Output != want {
		t.Fatalf("Output = %q\nwant      %q", plan.Output, want)
	}
	plan = build(t, "@daily root /opt/d.sh\n", true)
	if want := "@daily root " + wrapped("d", "0 0 * * *", "/bin/sh", "/opt/d.sh") + "\n"; plan.Output != want {
		t.Fatalf("macro: %q", plan.Output)
	}
}

func TestCronDLinesWithoutAUserOrACommandAreMalformed(t *testing.T) {
	for _, line := range []string{"0 2 * * * root", "0 2 * * *", "0 2 * * * bad!user /opt/x.sh"} {
		plan := build(t, line+"\n", true)
		if plan.Changed() || plan.Skipped != 1 || !strings.Contains(plan.Lines[0].Reason, "malformed") {
			t.Errorf("%q: plan = %+v", line, plan)
		}
	}
}

func TestMalformedLinesAreReportedAndNeverMangled(t *testing.T) {
	lines := []string{
		"0 2 * * /opt/x.sh",
		"this is not a cron line",
		"0 2 * * *",
		"0 2 * * *   ",
		"@weird /opt/x.sh",
		"@daily",
		"0 2 * * $(id) /opt/x.sh",
		"0 2 * 'x' * /opt/x.sh",
		"\x00\x01binary",
	}
	for _, line := range lines {
		plan := build(t, line+"\n", false)
		if plan.Output != line+"\n" || plan.Skipped != 1 {
			t.Errorf("%q: plan = %+v", line, plan)
		}
		if !strings.Contains(plan.Lines[0].Reason, "malformed") {
			t.Errorf("%q: reason = %q", line, plan.Lines[0].Reason)
		}
	}
}

func TestMalformedLineDoesNotStopOtherLines(t *testing.T) {
	plan := build(t, "garbage\n0 2 * * * /opt/x.sh\n", false)
	if plan.Wrapped != 1 || plan.Skipped != 1 || !strings.HasPrefix(plan.Output, "garbage\n0 2 * * * "+bin) {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestCRLFLineEndingsArePreserved(t *testing.T) {
	plan := build(t, "MAILTO=a\r\n0 2 * * * /opt/x.sh\r\n# c\r\n", false)
	want := "MAILTO=a\r\n0 2 * * * " + wrapped("x", "0 2 * * *", "/bin/sh", "/opt/x.sh") + "\r\n# c\r\n"
	if plan.Output != want {
		t.Fatalf("Output = %q\nwant      %q", plan.Output, want)
	}
}

func TestMissingFinalNewlineIsPreserved(t *testing.T) {
	plan := build(t, "0 2 * * * /opt/x.sh", false)
	if strings.HasSuffix(plan.Output, "\n") || plan.Wrapped != 1 {
		t.Fatalf("Output = %q", plan.Output)
	}
}

func TestVeryLongLinesAreSkippedBecauseCronMayDropThem(t *testing.T) {
	long := "0 2 * * * echo " + strings.Repeat("x", 3000)
	plan := build(t, long+"\n", false)
	if plan.Output != long+"\n" || plan.Skipped != 1 || !strings.Contains(plan.Lines[0].Reason, "1000") {
		t.Fatalf("plan = %+v", plan)
	}
	medium := "0 2 * * * echo " + strings.Repeat("x", 500)
	if plan := build(t, medium+"\n", false); plan.Wrapped != 1 {
		t.Fatalf("a 500 byte command fits and must be wrapped: %+v", plan)
	}
}

func TestShellAndTimeZoneFromEnvironmentLinesApplyToLaterJobs(t *testing.T) {
	in := "0 1 * * * /opt/before.sh\nSHELL=/bin/bash\nCRON_TZ=Europe/Istanbul\n0 2 * * * /opt/after.sh\n"
	plan := build(t, in, false)
	lines := strings.Split(plan.Output, "\n")
	if !strings.Contains(lines[0], "-- /bin/sh -c") || strings.Contains(lines[0], "--timezone") {
		t.Fatalf("line before the env lines: %q", lines[0])
	}
	if !strings.Contains(lines[3], "--schedule '0 2 * * *' --timezone 'Europe/Istanbul' -- /bin/bash -c '/opt/after.sh'") {
		t.Fatalf("line after the env lines: %q", lines[3])
	}
}

func TestUnsafeShellValueFallsBackToBinSh(t *testing.T) {
	plan := build(t, "SHELL=/bin/bash; rm -rf /\n0 2 * * * /opt/x.sh\n", false)
	if !strings.Contains(plan.Output, "-- /bin/sh -c") {
		t.Fatalf("Output = %q", plan.Output)
	}
}

func TestCommandsStartingWithADashAreSkipped(t *testing.T) {
	plan := build(t, "0 2 * * * -/opt/quiet.sh\n", false)
	if plan.Changed() || plan.Skipped != 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestBuildIsIdempotent(t *testing.T) {
	in := "MAILTO=a\n# c\n0 2 * * * /opt/a.sh\n@daily /opt/b.sh\n@reboot /opt/c.sh\n0 3 * * * date +%F\n* * * * * echo 'q'\n"
	first := build(t, in, false)
	second := build(t, first.Output, false)
	if second.Changed() || second.Output != first.Output {
		t.Fatalf("second pass changed the crontab:\n%s", second.Output)
	}
	if second.Wrapped != 0 {
		t.Fatalf("second.Wrapped = %d", second.Wrapped)
	}
}

func TestBinaryPathIsQuotedWhenNeededAndValidated(t *testing.T) {
	plan, err := Build("0 2 * * * /opt/x.sh\n", Options{BinaryPath: "/opt/my tools/cronbeat"})
	if err != nil || !strings.Contains(plan.Output, "'/opt/my tools/cronbeat' run --name x") {
		t.Fatalf("err=%v output=%q", err, plan.Output)
	}
	for _, bad := range []string{"", "relative/cronbeat", "/opt/100%/cronbeat", "/opt/new\nline/cronbeat"} {
		if _, err := Build("0 2 * * * /opt/x.sh\n", Options{BinaryPath: bad}); err == nil {
			t.Errorf("binary path %q was accepted", bad)
		}
	}
}

func TestReportFormatIsDeterministic(t *testing.T) {
	in := "# nightly\nMAILTO=a\n0 2 * * * /opt/backup.sh\n@reboot /opt/boot.sh\n0 3 * * * date +%F\nbroken line\n0 4 * * * /usr/local/bin/cronbeat run --name x -- y\n"
	got := build(t, in, false).Report()
	want := `line 3: wrap as "backup"
  - 0 2 * * * /opt/backup.sh
  + 0 2 * * * ` + wrapped("backup", "0 2 * * *", "/bin/sh", "/opt/backup.sh") + `
line 4: skip, @reboot has no schedule
line 5: skip, unescaped % (cron turns it into a newline)
line 6: skip, malformed crontab line
line 7: skip, already uses cronbeat
1 wrapped, 4 skipped, 2 unchanged.
`
	if got != want {
		t.Fatalf("Report =\n%s\nwant\n%s", got, want)
	}
}

func TestReportForNothingToDo(t *testing.T) {
	if got := build(t, "# only a comment\n", false).Report(); got != "0 wrapped, 0 skipped, 1 unchanged.\n" {
		t.Fatalf("Report = %q", got)
	}
}
