// SPDX-License-Identifier: GPL-2.0-only

// Package secret masks credentials in Live TV source URLs before a URL
// reaches a log line, an error string or a non-administrator response.
//
// IPTV panels put the account in every URL: the user name and password in
// the query of the playlist and guide URLs, and in the path of every channel
// URL (/live/<user>/<pass>/<id>.ts). A key-based rule sees the first form
// only. A Set therefore collects the secret values from the configured URLs
// and then masks those values wherever they appear (docs/spikes/livetv.md
// §4.3).
package secret

import (
	"cmp"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Mask replaces each secret value.
const Mask = "***"

// urlPattern finds absolute URLs inside free text such as an error string.
var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>` + "`" + `]+`)

// Set is a fixed list of secret values, longest first. The zero Set masks
// only the secrets that Redact finds in the text itself.
type Set struct {
	values []string
}

// Collect returns the secrets in the given sources. Each source is a URL or
// free text that contains URLs; extra values (a configured password, for
// example) are added as given. Empty values are ignored.
func Collect(sources []string, extra ...string) Set {
	var values []string
	for _, source := range sources {
		values = append(values, fromText(source)...)
	}
	values = append(values, extra...)
	return newSet(values)
}

// Redact masks every secret of the set, and every secret found in URLs in
// text, inside text.
func (s Set) Redact(text string) string {
	if text == "" {
		return text
	}
	values := s.values
	if found := fromText(text); len(found) > 0 {
		values = newSet(append(slices.Clone(s.values), found...)).values
	}
	for _, value := range values {
		for _, form := range forms(value) {
			text = strings.ReplaceAll(text, form, Mask)
		}
	}
	return text
}

// Err returns err with a redacted message. errors.Is and errors.As still see
// the original chain; a nil err stays nil.
func (s Set) Err(err error) error {
	if err == nil {
		return nil
	}
	message := s.Redact(err.Error())
	if message == err.Error() {
		return err
	}
	return &redactedError{message: message, err: err}
}

// Redact masks the secrets found in URLs inside text, plus the extra values.
func Redact(text string, extra ...string) string {
	return newSet(extra).Redact(text)
}

type redactedError struct {
	message string
	err     error
}

func (e *redactedError) Error() string { return e.message }

func (e *redactedError) Unwrap() error { return e.err }

// Is lets errors.Is match the wrapped chain without exposing its message.
func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }

func newSet(values []string) Set {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value == Mask {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	// Longest first, so that a secret that contains another is masked whole.
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	return Set{values: out}
}

// forms returns the spellings of value that can appear in a URL or a log
// line: raw, query-escaped and path-escaped.
func forms(value string) []string {
	out := []string{value}
	for _, escaped := range []string{url.QueryEscape(value), url.PathEscape(value)} {
		if escaped != value && !slices.Contains(out, escaped) {
			out = append(out, escaped)
		}
	}
	return out
}

func fromText(text string) []string {
	var values []string
	for _, raw := range urlPattern.FindAllString(text, -1) {
		values = append(values, fromURL(raw)...)
	}
	return values
}

// fromURL returns the user info, the values of secret query keys and the
// Xtream /<root>/<user>/<pass>/ path pair of one URL.
func fromURL(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	var values []string
	if parsed.User != nil {
		values = append(values, parsed.User.Username())
		if password, ok := parsed.User.Password(); ok {
			values = append(values, password)
		}
	}
	for key, entries := range parsed.Query() {
		if isSecretQueryKey(key) {
			values = append(values, entries...)
		}
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	for index := 0; index+3 < len(segments); index++ {
		if !isCredentialPathRoot(segments[index]) {
			continue
		}
		for _, segment := range segments[index+1 : index+3] {
			if decoded, err := url.PathUnescape(segment); err == nil {
				values = append(values, decoded)
			}
		}
	}
	return values
}

// isSecretQueryKey reports a query key whose value is a credential. The
// match is case-insensitive.
func isSecretQueryKey(key string) bool {
	switch strings.ToLower(key) {
	case "username", "user", "password", "pass", "pwd", "token", "key", "apikey",
		"api_key", "auth", "signature", "sig":
		return true
	}
	return false
}

// isCredentialPathRoot reports an Xtream path root that carries
// /<root>/<user>/<pass>/<id> stream URLs.
func isCredentialPathRoot(segment string) bool {
	switch strings.ToLower(segment) {
	case "live", "movie", "series", "timeshift":
		return true
	}
	return false
}
