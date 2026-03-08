package log

import (
	"context"
	"log/slog"
)

// topicPrefixes maps topic names to the "sub" attribute values they match.
var topicPrefixes = map[string][]string{
	"peer":       {"peer"},
	"download":   {"download", "dial"},
	"stream":     {"stream", "seek", "server"},
	"piece":      {"piece", "verifier", "endgame"},
	"pex":        {"pex"},
	"dht":        {"dht", "node", "magnet"},
	"tracker":    {"tracker"},
	"choker":     {"choker", "snub"},
	"protocol":   {"utp", "holepunch"},
	"upnp":       {"upnp"},
	"lsd":        {"lsd"},
	"stats":      {"stats"},
	"debug":      {"debug"},
	"bittorrent": {"bittorrent"},
	"seeding":    {"seeding"},
}

// TopicNames returns the list of known topic names (for completion).
func TopicNames() []string {
	names := make([]string, 0, len(topicPrefixes))
	for k := range topicPrefixes {
		names = append(names, k)
	}
	return names
}

// topicHandler wraps a slog.Handler and filters records by the "sub" attribute.
// Only records whose "sub" value matches an enabled topic pass through.
// Records without a "sub" attribute always pass through.
type topicHandler struct {
	inner   slog.Handler
	allowed map[string]bool // flattened set of allowed "sub" values
	all     bool            // true = pass everything
}

// newTopicHandler creates a filtering handler.
//   - nil/empty topics or a list containing "all" → pass everything.
//   - Otherwise only records with a matching "sub" attribute pass through.
func newTopicHandler(inner slog.Handler, topics []string) slog.Handler {
	if len(topics) == 0 {
		return inner // no filtering
	}

	allowed := make(map[string]bool)
	for _, t := range topics {
		if t == "all" {
			return inner
		}
		if subs, ok := topicPrefixes[t]; ok {
			for _, s := range subs {
				allowed[s] = true
			}
		}
	}

	if len(allowed) == 0 {
		return inner
	}

	return &topicHandler{inner: inner, allowed: allowed}
}

func (h *topicHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *topicHandler) Handle(ctx context.Context, r slog.Record) error {
	// Scan attributes for "sub" key.
	var sub string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "sub" {
			sub = a.Value.String()
			found = true
			return false
		}
		return true
	})

	// No "sub" attribute → always pass (errors, banners, etc.).
	if !found {
		return h.inner.Handle(ctx, r)
	}

	// Check if this sub is in the allowed set.
	if h.allowed[sub] {
		return h.inner.Handle(ctx, r)
	}

	// Filtered out.
	return nil
}

func (h *topicHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &topicHandler{
		inner:   h.inner.WithAttrs(attrs),
		allowed: h.allowed,
	}
}

func (h *topicHandler) WithGroup(name string) slog.Handler {
	return &topicHandler{
		inner:   h.inner.WithGroup(name),
		allowed: h.allowed,
	}
}
