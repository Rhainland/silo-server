package markers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func overlayFile(intro, credits *[2]float64, introSource, creditsSource string) *models.MediaFile {
	f := &models.MediaFile{ID: 7}
	if intro != nil {
		f.IntroStart, f.IntroEnd = new(intro[0]), new(intro[1])
		f.IntroMarkersSource = new(introSource)
		f.MarkerSegments = append(f.MarkerSegments, models.MarkerSegment{Kind: "intro", StartSeconds: intro[0], EndSeconds: intro[1]})
	}
	if credits != nil {
		f.CreditsStart, f.CreditsEnd = new(credits[0]), new(credits[1])
		f.CreditsMarkersSource = new(creditsSource)
		f.MarkerSegments = append(f.MarkerSegments, models.MarkerSegment{Kind: "credits", StartSeconds: credits[0], EndSeconds: credits[1]})
	}
	return f
}

// An on-demand online intro is never saved, so it is laid over the stored
// row, while credits detection just saved comes from storage.
func TestOverlayOnlineKeepsUnsavedOnlineMarkers(t *testing.T) {
	view := overlayFile(&[2]float64{20, 80}, nil, models.MarkerSourceOnline, "")
	stored := overlayFile(nil, &[2]float64{1700, 1780}, "", models.MarkerSourceScanner)

	got := OverlayOnline(stored, view)
	if got.IntroStart == nil || *got.IntroStart != 20 || *got.IntroMarkersSource != models.MarkerSourceOnline {
		t.Fatalf("intro = %v, want the online intro", got.IntroStart)
	}
	if got.CreditsStart == nil || *got.CreditsStart != 1700 {
		t.Fatalf("credits = %v, want the stored credits", got.CreditsStart)
	}
	if len(got.MarkerSegments) != 2 {
		t.Fatalf("segments = %+v, want intro and credits", got.MarkerSegments)
	}
	if stored.IntroStart != nil {
		t.Fatal("OverlayOnline modified stored")
	}
}

// Markers edited or cleared after the view was read are not undone: only
// online markers are laid over, and never over a higher-priority one.
func TestOverlayOnlineDoesNotReplayStaleMarkers(t *testing.T) {
	view := overlayFile(&[2]float64{10, 50}, &[2]float64{1700, 1780}, models.MarkerSourceScanner, models.MarkerSourceOnline)
	stored := overlayFile(nil, &[2]float64{1690, 1770}, "", models.MarkerSourceManual)

	got := OverlayOnline(stored, view)
	if got.IntroStart != nil {
		t.Fatalf("a cleared detector intro came back: %v", *got.IntroStart)
	}
	if *got.CreditsStart != 1690 || *got.CreditsMarkersSource != models.MarkerSourceManual {
		t.Fatalf("credits = %v from %v, want the editor's credits", *got.CreditsStart, *got.CreditsMarkersSource)
	}
}

func TestOverlayOnlineIgnoresAnotherFile(t *testing.T) {
	view := overlayFile(&[2]float64{20, 80}, nil, models.MarkerSourceOnline, "")
	view.ID = 8
	stored := overlayFile(nil, nil, "", "")
	if got := OverlayOnline(stored, view); got.IntroStart != nil {
		t.Fatal("markers from another file were laid over")
	}
}
