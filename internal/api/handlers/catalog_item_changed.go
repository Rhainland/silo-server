package handlers

import (
	"context"
	"log/slog"

	"github.com/Silo-Server/silo-server/internal/cache"
	"github.com/Silo-Server/silo-server/internal/sections"
)

// publishCatalogItemChanged runs after an admin changes items' identity,
// fields or artwork outside a scan. It evicts the cached home rails that list
// them on this node at once, so the client's next read is fresh, and tells the
// other API nodes to do the same. A match passes both the old and the new
// content ID, because the cached rails still hold the old one.
func publishCatalogItemChanged(ctx context.Context, bus cache.EventBus, contentIDs ...string) {
	sections.EvictResolvedListItems(contentIDs...)
	if bus == nil {
		return
	}
	published := make(map[string]struct{}, len(contentIDs))
	for _, id := range contentIDs {
		if _, done := published[id]; done || id == "" {
			continue
		}
		published[id] = struct{}{}
		if err := bus.Publish(ctx, cache.ChannelCatalog, cache.Event{Type: cache.EventCatalogItemChanged, Payload: id}); err != nil {
			slog.WarnContext(ctx, "admin: failed to publish catalog item change", "component", "api", "content_id", id, "error", err)
		}
	}
}
