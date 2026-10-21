// Package redact masks secrets in text that goes to logs and messages.
// It is a second line of defense: code should not pass secrets to logs in the first place.
package redact

import (
	"context"
	"log/slog"
	"regexp"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`ept_[0-9a-f]{12}_[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`epc1\.[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// String returns the text with secrets masked.
func String(s string) string {
	for _, p := range patterns {
		s = p.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// Handler wraps a slog.Handler and masks secrets in messages and string values.
type Handler struct{ slog.Handler }

func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(clean(a))
		return true
	})
	return h.Handler.Handle(ctx, nr)
}

func (h Handler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = clean(a)
	}
	return Handler{h.Handler.WithAttrs(out)}
}

func (h Handler) WithGroup(name string) slog.Handler { return Handler{h.Handler.WithGroup(name)} }

func clean(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, String(err.Error()))
		}
	}
	return a
}
