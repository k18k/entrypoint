// Pakiet redact maskuje sekrety w tekście trafiającym do logów i komunikatów.
// To druga linia obrony: kod i tak nie powinien przekazywać sekretów do logów.
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

// String zwraca tekst z zamaskowanymi sekretami.
func String(s string) string {
	for _, p := range patterns {
		s = p.ReplaceAllString(s, "[ukryte]")
	}
	return s
}

// Handler opakowuje slog.Handler i maskuje sekrety w komunikatach i wartościach tekstowych.
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
