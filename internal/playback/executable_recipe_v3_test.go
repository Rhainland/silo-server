package playback

import "testing"

func TestExecutableRecipeV3RoundTripPreservesOperationalFields(t *testing.T) {
	plan := &PlanV3{PlanID: "plan:frozen", Delivery: DeliveryRemuxHLSV3}
	want := PlannerResultV3{
		Plan: plan, PlayMethod: PlayRemux, TranscodeAudio: true,
		TargetVideoCodec: "copy", TargetAudioCodec: "aac", TargetAudioChannels: 6,
		TargetResolution: "1080p", TargetBitrateKbps: 18_000,
		SourceVideoCodec: "hevc", SourceDurationSeconds: 7_201,
		SubtitleTrackIndex: 4, SubtitleTransportTrackIndex: 2,
		SubtitleBurnIn: true, SubtitleCodec: "hdmv_pgs_subtitle",
	}
	recipe := FreezeExecutableRecipeV3(want)
	if !recipe.Valid() {
		t.Fatalf("frozen recipe is invalid: %#v", recipe)
	}
	if !recipe.ValidFor(*plan) {
		t.Fatalf("frozen recipe does not match its plan: %#v", recipe)
	}
	changedPlan := *plan
	changedPlan.PlanID = "plan:newer"
	if recipe.ValidFor(changedPlan) {
		t.Fatal("stale frozen recipe matched a newer plan")
	}
	got := recipe.PlannerResult(plan)
	if got.Plan != plan || got.PlayMethod != want.PlayMethod || got.TranscodeAudio != want.TranscodeAudio ||
		got.TargetVideoCodec != want.TargetVideoCodec || got.TargetAudioCodec != want.TargetAudioCodec ||
		got.TargetAudioChannels != want.TargetAudioChannels || got.TargetResolution != want.TargetResolution ||
		got.TargetBitrateKbps != want.TargetBitrateKbps || got.SubtitleTrackIndex != want.SubtitleTrackIndex ||
		got.SubtitleTransportTrackIndex != want.SubtitleTransportTrackIndex || got.SubtitleBurnIn != want.SubtitleBurnIn ||
		got.SubtitleCodec != want.SubtitleCodec || !got.FrozenSourceMetadata ||
		got.SourceVideoCodec != want.SourceVideoCodec || got.SourceDurationSeconds != want.SourceDurationSeconds {
		t.Fatalf("thawed result = %#v, want %#v", got, want)
	}
}
