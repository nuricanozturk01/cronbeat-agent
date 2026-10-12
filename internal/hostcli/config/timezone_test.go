package config

import (
	"errors"
	"testing"
)

func zoneSources(env map[string]string, files map[string]string, links map[string]string) TimeZoneSources {
	return TimeZoneSources{
		Getenv: func(k string) string { return env[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("absent")
		},
		Readlink: func(p string) (string, error) {
			if v, ok := links[p]; ok {
				return v, nil
			}
			return "", errors.New("absent")
		},
	}
}

func TestDetectTimeZone(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		files map[string]string
		links map[string]string
		want  string
	}{
		{"TZ variable", map[string]string{"TZ": "Europe/Istanbul"}, nil, nil, "Europe/Istanbul"},
		{"TZ with colon", map[string]string{"TZ": ":America/New_York"}, nil, nil, "America/New_York"},
		{"TZ UTC", map[string]string{"TZ": "UTC"}, nil, nil, "UTC"},
		{"TZ beats files", map[string]string{"TZ": "Asia/Tokyo"}, map[string]string{"/etc/timezone": "Europe/Berlin\n"}, nil, "Asia/Tokyo"},
		{"invalid TZ falls back to the file", map[string]string{"TZ": "<+03>-3"}, map[string]string{"/etc/timezone": "Europe/Berlin\n"}, nil, "Europe/Berlin"},
		{"etc timezone", nil, map[string]string{"/etc/timezone": "  Europe/Berlin \n"}, nil, "Europe/Berlin"},
		{"localtime symlink", nil, nil, map[string]string{"/etc/localtime": "/usr/share/zoneinfo/Europe/Paris"}, "Europe/Paris"},
		{"relative symlink", nil, nil, map[string]string{"/etc/localtime": "../usr/share/zoneinfo/Etc/UTC"}, "Etc/UTC"},
		{"posix prefix", nil, nil, map[string]string{"/etc/localtime": "/usr/share/zoneinfo/posix/Europe/Paris"}, "Europe/Paris"},
		{"garbage in file", nil, map[string]string{"/etc/timezone": "not a zone!\n"}, nil, ""},
		{"nothing", nil, nil, nil, ""},
		{"injection attempt", map[string]string{"TZ": "UTC'; rm -rf /"}, nil, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectTimeZone(zoneSources(tt.env, tt.files, tt.links)); got != tt.want {
				t.Fatalf("DetectTimeZone = %q, want %q", got, tt.want)
			}
		})
	}
}
