package markers

import (
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// OverlayOnline returns a copy of stored with the online markers of view laid
// over it. view is an earlier read of the same file that may carry on-demand
// provider markers, which are never saved. Every other marker comes from
// stored, so an edit or clear made after view was read is not undone. An
// online marker replaces a stored one only when its source outranks it.
func OverlayOnline(stored, view *models.MediaFile) *models.MediaFile {
	if stored == nil {
		return view
	}
	merged := *stored
	if view == nil || view.ID != stored.ID {
		return &merged
	}
	mergedFields, viewFields, storedFields := segmentFieldsOf(&merged), segmentFieldsOf(view), segmentFieldsOf(stored)
	overlaid := map[string]bool{}
	for i, from := range viewFields {
		source := from.effectiveSource(view)
		if source != models.MarkerSourceOnline && source != models.MarkerSourcePlugin {
			continue
		}
		if storedFields[i].present() &&
			models.MarkerSourcePriority(storedFields[i].effectiveSource(stored)) >= models.MarkerSourcePriority(source) {
			continue
		}
		mergedFields[i].copyFrom(from)
		overlaid[from.kind] = true
	}
	if len(overlaid) == 0 {
		return &merged
	}
	segments := make([]models.MarkerSegment, 0, len(stored.MarkerSegments)+len(view.MarkerSegments))
	for _, segment := range stored.MarkerSegments {
		if !overlaid[segment.Kind] {
			segments = append(segments, segment)
		}
	}
	for _, segment := range view.MarkerSegments {
		if overlaid[segment.Kind] {
			segments = append(segments, segment)
		}
	}
	merged.MarkerSegments = segments
	return &merged
}

// Segment kind names as stored in marker_segments.
const (
	segmentKindIntro   = "intro"
	segmentKindCredits = "credits"
	segmentKindRecap   = "recap"
	segmentKindPreview = "preview"
)

// segmentFields addresses one marker kind's columns on a file.
type segmentFields struct {
	kind             string
	start, end       **float64
	source, provider **string
	confidence       **float64
	algorithm        **string
	detectedAt       **time.Time
}

func segmentFieldsOf(f *models.MediaFile) []segmentFields {
	return []segmentFields{
		{segmentKindIntro, &f.IntroStart, &f.IntroEnd, &f.IntroMarkersSource, &f.IntroMarkersProvider, &f.IntroMarkersConfidence, &f.IntroMarkersAlgorithm, &f.IntroMarkersDetectedAt},
		{segmentKindCredits, &f.CreditsStart, &f.CreditsEnd, &f.CreditsMarkersSource, &f.CreditsMarkersProvider, &f.CreditsMarkersConfidence, &f.CreditsMarkersAlgorithm, &f.CreditsMarkersDetectedAt},
		{segmentKindRecap, &f.RecapStart, &f.RecapEnd, &f.RecapMarkersSource, &f.RecapMarkersProvider, &f.RecapMarkersConfidence, &f.RecapMarkersAlgorithm, &f.RecapMarkersDetectedAt},
		{segmentKindPreview, &f.PreviewStart, &f.PreviewEnd, &f.PreviewMarkersSource, &f.PreviewMarkersProvider, &f.PreviewMarkersConfidence, &f.PreviewMarkersAlgorithm, &f.PreviewMarkersDetectedAt},
	}
}

func (s segmentFields) present() bool { return *s.start != nil && *s.end != nil }

// effectiveSource is the segment's own source, or the file's shared legacy
// source for a segment written before per-segment provenance.
func (s segmentFields) effectiveSource(file *models.MediaFile) string {
	if *s.source != nil && strings.TrimSpace(**s.source) != "" {
		return strings.TrimSpace(**s.source)
	}
	if s.present() && file.MarkersSource != nil {
		return strings.TrimSpace(*file.MarkersSource)
	}
	return ""
}

func (s segmentFields) copyFrom(from segmentFields) {
	*s.start, *s.end = *from.start, *from.end
	*s.source, *s.provider = *from.source, *from.provider
	*s.confidence, *s.algorithm, *s.detectedAt = *from.confidence, *from.algorithm, *from.detectedAt
}
