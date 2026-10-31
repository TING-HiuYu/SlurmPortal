package mail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Worker/wasm has no system zoneinfo.
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxSubjectBytes  = 512
	maxHTMLBytes     = 65536
	maxRenderedBytes = 262144
	DefaultTimeZone  = "Asia/Shanghai"
)

var (
	variablePattern = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}`)
	constantKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	zonePattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._+-]*(?:/[A-Za-z0-9._+-]+)*$`)
	utcPattern      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z$`)
	breakTag        = regexp.MustCompile(`(?i)^(?:br|/p|/div|/li)\b`)
	lineBreaks      = regexp.MustCompile("[\r\n\x00]+")
	entity          = regexp.MustCompile(`&(amp|lt|gt|quot|#39|nbsp);`)
	htmlEscaper     = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	entities        = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "#39": "'", "nbsp": " "}
	forbidden       = Roots("__proto__", "prototype", "constructor")
)

// JSLen counts UTF-16 code units, matching browser string length limits.
func JSLen(s string) int { return len(utf16.Encode([]rune(s))) }

// Template is an admin-edited mail template: {{ path.to.value }} placeholders, HTML-escaped in the body.
type Template struct {
	Subject   string         `json:"subject"`
	HTML      string         `json:"html"`
	Constants map[string]any `json:"constants"`
}

// Validate checks a template whose {{ var.path }} placeholders may only start at one of roots.
func (t Template) Validate(roots map[string]bool) (Template, error) {
	if strings.TrimSpace(t.Subject) == "" || len(t.Subject) > maxSubjectBytes || strings.ContainsAny(t.Subject, "\r\n\x00") {
		return t, invalid("invalid subject")
	}
	if strings.TrimSpace(t.HTML) == "" || len(t.HTML) > maxHTMLBytes || strings.Contains(t.HTML, "\x00") {
		return t, invalid("invalid HTML template")
	}
	if len(t.Constants) > 64 {
		return t, invalid("invalid constants")
	}
	constants := make(map[string]any, len(t.Constants))
	for k, v := range t.Constants {
		switch v.(type) {
		case string, float64, bool:
		default:
			return t, invalid("invalid constant")
		}
		s := Stringify(v)
		if !constantKey.MatchString(k) || forbidden[k] || len(s) > 4096 || strings.Contains(s, "\x00") {
			return t, invalid("invalid constant")
		}
		constants[k] = v
	}
	for _, src := range []string{t.Subject, t.HTML} {
		for _, m := range variablePattern.FindAllStringSubmatch(src, -1) {
			parts := strings.Split(m[1], ".")
			if !roots[parts[0]] {
				return t, invalid("invalid template variable")
			}
			for _, p := range parts {
				if p == "" || forbidden[p] {
					return t, invalid("invalid template variable")
				}
			}
		}
		rest := variablePattern.ReplaceAllString(src, "")
		if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
			return t, invalid("unsupported template syntax")
		}
	}
	return Template{Subject: t.Subject, HTML: t.HTML, Constants: constants}, nil
}

// NormalizeTimeZone accepts an IANA zone name.
func NormalizeTimeZone(v string) (string, error) {
	z := strings.TrimSpace(v)
	if JSLen(v) > 100 || !zonePattern.MatchString(z) || z == "Local" {
		return "", invalid("invalid time zone")
	}
	if _, err := time.LoadLocation(z); err != nil {
		return "", invalid("invalid time zone")
	}
	return z, nil
}

// jsNumber formats like JavaScript's String(number).
func jsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if a := math.Abs(f); a >= 1e21 || a < 1e-6 {
		s := strconv.FormatFloat(f, 'g', -1, 64)
		return strings.NewReplacer("e-0", "e-", "e+0", "e+").Replace(s)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Stringify renders a template value as text: nil is empty, scalars print plainly, anything else as JSON.
func Stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return jsNumber(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return jsNumber(f)
		}
		return x.String()
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func valueAt(ctx map[string]any, path string) string {
	var v any = ctx
	for _, p := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[p]
			if !ok {
				return ""
			}
			v = next
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != p {
				return ""
			}
			v = x[i]
		default:
			return ""
		}
	}
	return Stringify(v)
}

// DurationText formats seconds as H:MM:SS.
func DurationText(s int64) string {
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// LocalTime renders an RFC3339 UTC timestamp in loc, or "" when v is not one.
func LocalTime(v any, loc *time.Location, zone string) string {
	s, ok := v.(string)
	if !ok || !utcPattern.MatchString(s) || strings.HasPrefix(s, "0000") {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	t = t.In(loc)
	if t.Year() < 0 || t.Year() > 9999 {
		return ""
	}
	return t.Format("2006-01-02 15:04:05") + " (" + zone + ")"
}

func firstBytes(s string, n int) string {
	size := 0
	for i, r := range s {
		size += utf8.RuneLen(r)
		if size > n {
			return s[:i]
		}
	}
	return s
}

func renderBounded(src string, ctx map[string]any, html bool) (string, error) {
	limit := maxSubjectBytes
	if html {
		limit = maxRenderedBytes
	}
	var out strings.Builder
	truncated := ""
	cut := false
	appendPart := func(v string) bool {
		if out.Len()+len(v) > limit {
			if !html {
				truncated = firstBytes(out.String(), 509) + firstBytes(v, max(0, 509-out.Len())) + "…"
			}
			cut = true
			return false
		}
		out.WriteString(v)
		return true
	}
	prev := 0
	for _, m := range variablePattern.FindAllStringSubmatchIndex(src, -1) {
		if !appendPart(src[prev:m[0]]) {
			break
		}
		v := valueAt(ctx, src[m[2]:m[3]])
		if html {
			v = htmlEscaper.Replace(v)
		} else {
			v = lineBreaks.ReplaceAllString(v, " ")
		}
		if !appendPart(v) {
			break
		}
		prev = m[1]
	}
	if !cut {
		appendPart(src[prev:])
	}
	if cut {
		if html {
			return "", invalid("rendered mail exceeds limits")
		}
		return truncated, nil
	}
	return out.String(), nil
}

// PlainText derives the text/plain part from rendered HTML.
func PlainText(html string) string {
	var b strings.Builder
	pos := 0
	for pos < len(html) {
		open := strings.IndexByte(html[pos:], '<')
		if open < 0 {
			b.WriteString(html[pos:])
			break
		}
		open += pos
		b.WriteString(html[pos:open])
		end := strings.IndexByte(html[open+1:], '>')
		if end < 0 {
			b.WriteString(html[open:])
			break
		}
		end += open + 1
		if breakTag.MatchString(html[open+1 : end]) {
			b.WriteByte('\n')
		}
		pos = end + 1
	}
	return entity.ReplaceAllStringFunc(b.String(), func(m string) string { return entities[m[1:len(m)-1]] })
}

// Variable documents one placeholder for template authors.
type Variable struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Rendered is a template filled with one context.
type Rendered struct {
	Subject   string     `json:"subject"`
	HTML      string     `json:"html"`
	Text      string     `json:"text"`
	Variables []Variable `json:"variables"`
}

// Roots builds the set of allowed top-level placeholder names.
func Roots(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// Render validates t against roots and fills it from ctx.
func Render(t Template, roots map[string]bool, ctx map[string]any, vars []Variable) (Rendered, error) {
	t, err := t.Validate(roots)
	if err != nil {
		return Rendered{}, err
	}
	subject, err := renderBounded(t.Subject, ctx, false)
	if err != nil {
		return Rendered{}, err
	}
	html, err := renderBounded(t.HTML, ctx, true)
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{Subject: subject, HTML: html, Text: PlainText(html), Variables: vars}, nil
}
