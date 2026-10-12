package schedule

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"five fields", "0 2 * * *", "0 2 * * *", false},
		{"extra whitespace and tabs", "  0 \t 2   *\t* *  ", "0 2 * * *", false},
		{"steps ranges lists", "*/5 9-17 1,15 * mon-fri", "*/5 9-17 1,15 * mon-fri", false},
		{"daily", "@daily", "0 0 * * *", false},
		{"midnight", "@midnight", "0 0 * * *", false},
		{"hourly", "@hourly", "0 * * * *", false},
		{"weekly", "@weekly", "0 0 * * 0", false},
		{"monthly", "@monthly", "0 0 1 * *", false},
		{"yearly", "@yearly", "0 0 1 1 *", false},
		{"annually", "@annually", "0 0 1 1 *", false},
		{"macro is case insensitive", "@DAILY", "0 0 * * *", false},
		{"reboot has no schedule", "@reboot", "", true},
		{"unknown macro", "@every5m", "", true},
		{"four fields", "0 2 * *", "", true},
		{"six fields", "0 0 2 * * *", "", true},
		{"empty", "", "", true},
		{"only spaces", "   ", "", true},
		{"newline inside", "0 2 * *\n*", "", true},
		{"quote injection", "0 2 * * 'x", "", true},
		{"shell metacharacter", "0 2 * * $(id)", "", true},
		{"letters in the minute field", "this is not a cron", "", true},
		{"hash is not supported", "0 0 * * mon#2", "", true},
		{"too long", string(make([]byte, 300)), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsReboot(t *testing.T) {
	if !IsReboot("@reboot") || !IsReboot(" @REBOOT ") || IsReboot("@daily") || IsReboot("0 0 * * *") {
		t.Fatal("IsReboot misclassifies")
	}
}
