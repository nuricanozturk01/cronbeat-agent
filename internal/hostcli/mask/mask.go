// Package mask removes secrets and personal data from text before it leaves the host.
// The patterns and their order are the same as the backend SecretMasker, and both are
// tested against testdata/mask_vectors.json.
package mask

import (
	"regexp"
	"strings"
)

const (
	replacement = "***"
	space       = ` \t\n\x0B\f\r`
	notSpace    = `[^` + space + `]`
)

var (
	urlCredentials = regexp.MustCompile(`(://)[^` + space + `/@:]+:[^` + space + `/@]+@`)
	authorization  = regexp.MustCompile(
		`(` + fold("authorization") + `[` + space + `]*[:=][` + space + `]*)(` +
			fold("bearer") + `[` + space + `]+|` + fold("basic") + `[` + space + `]+)?` + notSpace + `+`)
	bearer   = regexp.MustCompile(`\b` + fold("bearer") + `[` + space + `]+` + notSpace + `+`)
	keyValue = regexp.MustCompile(
		`\b(\w*(?:` + fold("password") + `|` + fold("passwd") + `|` + fold("pwd") + `|` + fold("secret") +
			`|` + fold("token") + `|` + fold("api") + `[_-]?` + fold("key") + `|` + fold("access") + `[_-]?` +
			fold("key") + `)\w*[` + space + `]*[:=][` + space + `]*)[^` + space + `,;&"']+`)
	email     = regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`)
	longToken = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)
)

// fold makes a literal ASCII-case-insensitive. Go's (?i) also folds non-ASCII
// letters such as the Kelvin sign, which the Java (?i) used by the backend does not.
func fold(literal string) string {
	var b strings.Builder
	for _, r := range literal {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteString("[" + string(r) + string(r-'a'+'A') + "]")
		case r >= 'A' && r <= 'Z':
			b.WriteString("[" + string(r) + string(r-'A'+'a') + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Text masks secrets in s.
func Text(s string) string {
	s = urlCredentials.ReplaceAllString(s, "${1}"+replacement+"@")
	s = authorization.ReplaceAllString(s, "${1}"+replacement)
	s = bearer.ReplaceAllString(s, "Bearer "+replacement)
	s = keyValue.ReplaceAllString(s, "${1}"+replacement)
	s = email.ReplaceAllString(s, replacement)
	return longToken.ReplaceAllString(s, replacement)
}
