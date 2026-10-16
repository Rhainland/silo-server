package handlers

import (
	"slices"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func subtitleRenditionPlanV3(file *models.MediaFile, delivery playback.DeliveryV3, mode playback.SubtitleModeV3) *playback.PlanV3 {
	return &playback.PlanV3{
		Delivery: delivery,
		Subtitle: playback.SubtitleDecisionV3{
			Mode: mode, TrackID: playback.TrackIDV3(file.ID, "subtitle", 0),
			HLSRendition: &playback.HLSSubtitleRenditionV3{GroupID: playback.SubtitleRenditionGroupID, Name: playback.SubtitleRenditionName, Format: "vtt", MIMEType: "text/vtt"},
			Artifact:     &playback.SubtitleArtifactV3{URL: "/stale.vtt", Format: "vtt"},
			Inventory:    playback.BuildSubtitleInventoryV3(file, nil),
		},
	}
}

func TestAttachSubtitleArtifactKeepsHLSSubtitleRendition(t *testing.T) {
	file := &models.MediaFile{ID: 42, SubtitleTracks: []models.SubtitleTrack{{Index: 4, Codec: "subrip"}}}
	plan := subtitleRenditionPlanV3(file, playback.DeliveryTranscodeHLSV3, playback.SubtitleRenderV3)
	handler := &PlaybackHandler{}
	if err := handler.attachSubtitleArtifactV3(t.Context(), "session", file, plan, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	if plan.Subtitle.HLSRendition == nil || plan.Subtitle.Artifact != nil {
		t.Fatalf("an in-manifest subtitle publishes no sidecar artifact: %#v", plan.Subtitle)
	}
	if plan.Subtitle.Inventory[0].URL == "" {
		t.Fatal("the inventory keeps its sidecar URL for other selections")
	}
}

func TestAttachSubtitleArtifactRejectsOrClearsMisplacedRendition(t *testing.T) {
	file := &models.MediaFile{ID: 42, SubtitleTracks: []models.SubtitleTrack{{Index: 4, Codec: "subrip"}}}
	handler := &PlaybackHandler{}
	for _, delivery := range []playback.DeliveryV3{playback.DeliveryRemuxHLSV3, playback.DeliveryOriginalHTTPV3} {
		plan := subtitleRenditionPlanV3(file, delivery, playback.SubtitleRenderV3)
		if err := handler.attachSubtitleArtifactV3(t.Context(), "session", file, plan, 0, nil, nil); err == nil {
			t.Fatalf("%s accepted an HLS subtitle rendition", delivery)
		}
	}
	plan := subtitleRenditionPlanV3(file, playback.DeliveryTranscodeHLSV3, playback.SubtitleOffV3)
	if err := handler.attachSubtitleArtifactV3(t.Context(), "session", file, plan, -1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if plan.Subtitle.HLSRendition != nil {
		t.Fatalf("subtitles-off plan kept a rendition: %#v", plan.Subtitle)
	}
}

func TestSidecarOnlyHLSReplanReusesTransportOnlyForAnUnchangedRendition(t *testing.T) {
	rendition := &playback.HLSSubtitleRenditionV3{GroupID: "subs", Name: "Subtitles", Format: "vtt", MIMEType: "text/vtt"}
	freeze := func(plan *playback.PlanV3, track int, withRendition bool) playback.ExecutableRecipeV3 {
		return playback.FreezeExecutableRecipeV3(playback.PlannerResultV3{
			Plan: plan, PlayMethod: playback.PlayTranscode, TargetVideoCodec: "h264", TargetAudioCodec: "aac",
			SubtitleTrackIndex: track, SubtitleTransportTrackIndex: track, SubtitleCodec: "subrip", SubtitleRendition: withRendition,
		})
	}
	for _, tc := range []struct {
		name                       string
		current, candidate         bool
		currentTrack, candidateTrk int
		want                       bool
	}{
		{"sidecar to rendition", false, true, 0, 0, false},
		{"rendition to another track", true, true, 0, 1, false},
		{"rendition kept", true, true, 0, 0, true},
		{"rendition dropped", true, false, 0, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentPlan := playback.PlanV3{PlanID: "current-plan", Delivery: playback.DeliveryTranscodeHLSV3,
				Stream: playback.StreamV3{URL: "/stream/current.m3u8"}, RequestedMediaFileID: 1, EffectiveMediaFileID: 1}
			candidatePlan := currentPlan
			candidatePlan.PlanID = "candidate-plan"
			if tc.current {
				currentPlan.Subtitle.HLSRendition = rendition
			}
			if tc.candidate {
				candidatePlan.Subtitle.HLSRendition = rendition
			}
			record := &playback.AttemptRecordV3{EffectiveMediaFileID: 1, CurrentPlan: currentPlan,
				FrozenRecipe: freeze(&currentPlan, tc.currentTrack, tc.current)}
			record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID = "output-context"
			_, reused := sidecarOnlyHLSReplanV3(record, &candidatePlan, freeze(&candidatePlan, tc.candidateTrk, tc.candidate), "output-context")
			if reused != tc.want {
				t.Fatalf("reused = %t, want %t", reused, tc.want)
			}
		})
	}
}

func TestHLSSubtitleRenditionsAreNegotiatedOnlyOnAPIV2(t *testing.T) {
	if slices.Contains(playback.ServerFeaturesV3(), playback.FeatureHLSSubtitleRenditionsV3) {
		t.Fatal("the frozen /api/v1 surface must not advertise hls_subtitle_renditions_v1")
	}
	if !slices.Contains(playback.NativeServerFeaturesV3(), playback.FeatureHLSSubtitleRenditionsV3) ||
		!slices.Contains(playback.AttemptStickyFeaturesV3(), playback.FeatureHLSSubtitleRenditionsV3) {
		t.Fatal("/api/v2 advertises hls_subtitle_renditions_v1 as an attempt-sticky feature")
	}
}

func TestFailedHLSSubtitleRenditionStaysOffForTheAttempt(t *testing.T) {
	negotiated := []string{playback.FeaturePlaybackPlanV3, playback.FeatureHLSSubtitleRenditionsV3}
	features := withoutFailedSubtitleRoutesV3(negotiated, negotiated, playback.FailureSubtitleHLSRenditionFailedV3)
	if playback.HasFeatureV3(features, playback.FeatureHLSSubtitleRenditionsV3) {
		t.Fatal("a rendition the player could not select is offered again")
	}
	// Pinning on the next replan restores sticky features from the durable
	// request, which no longer carries the rendition.
	if playback.HasFeatureV3(playback.PinAttemptStickyFeaturesV3(negotiated, features), playback.FeatureHLSSubtitleRenditionsV3) {
		t.Fatal("attempt-sticky pinning re-enabled the failed rendition")
	}
	for _, classification := range []string{"", "network_error", "subtitle_embedded_failed"} {
		if !playback.HasFeatureV3(withoutFailedSubtitleRoutesV3(negotiated, negotiated, classification), playback.FeatureHLSSubtitleRenditionsV3) {
			t.Fatalf("%q dropped the rendition", classification)
		}
	}
}
