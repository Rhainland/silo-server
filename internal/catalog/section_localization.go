package catalog

import (
	"context"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

// LocalizeSectionItems localizes home and library section cards for the
// viewer in batches: movies, series and other media items through their item
// localizations, episode cards through their episode localizations. It returns
// clones in input order, because section items can come from a
// process-global cache and must never carry one profile's text, plus the
// language each non-episode item's description is still missing (the detail
// document's pending_translation_language rule), keyed by content ID.
func (s *DetailService) LocalizeSectionItems(ctx context.Context, items []*models.MediaItem, filter AccessFilter) ([]*models.MediaItem, map[string]string, error) {
	localized := make([]*models.MediaItem, len(items))
	pending := make(map[string]string)
	var media []*models.MediaItem
	var episodeIDs []string
	for i, item := range items {
		localized[i] = cloneMediaItem(item)
		if item == nil || item.ContentID == "" {
			continue
		}
		if item.Type == recentTVTypeEpisode {
			episodeIDs = append(episodeIDs, item.ContentID)
		} else {
			media = append(media, item)
		}
	}

	if len(media) > 0 && s.itemLocRepo != nil {
		targets, locs, err := s.loadItemLocalizations(ctx, media, filter)
		if err != nil {
			return localized, pending, err
		}
		for i, item := range items {
			if item == nil || item.Type == recentTVTypeEpisode {
				continue
			}
			language, loc := targets[item.ContentID], locs[item.ContentID]
			localized[i] = s.localizeItemModelWith(item, language, loc)
			if language := pendingTranslationLanguageWith(item, language, loc); language != "" {
				pending[item.ContentID] = language
			}
		}
	}

	if len(episodeIDs) > 0 && s.episodeRepo != nil && s.episodeLocRepo != nil {
		episodes, err := s.episodeRepo.GetByIDs(ctx, episodeIDs)
		if err != nil {
			return localized, pending, err
		}
		localizedEpisodes, err := s.LocalizeEpisodeModels(ctx, episodes, filter)
		if err != nil {
			return localized, pending, err
		}
		byID := make(map[string]*models.Episode, len(localizedEpisodes))
		for _, episode := range localizedEpisodes {
			if episode != nil {
				byID[episode.ContentID] = episode
			}
		}
		for i, item := range items {
			if item == nil || item.Type != recentTVTypeEpisode {
				continue
			}
			episode := byID[item.ContentID]
			if episode == nil {
				continue
			}
			// An empty localized or base title keeps the card's own title.
			if title := strings.TrimSpace(episode.Title); title != "" {
				localized[i].Title = title
			}
			if strings.TrimSpace(episode.Overview) != "" {
				localized[i].Overview = episode.Overview
			}
			localized[i].MachineTranslatedFields = episode.MachineTranslatedFields
		}
	}
	return localized, pending, nil
}
