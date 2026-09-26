package intromarkers

import (
	"regexp"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

var introChapterPattern = regexp.MustCompile(`(?i)(^|\s)(intro|introduction|opening)(\s|:|$)`)
var generatedChapterPattern = regexp.MustCompile(`(?i)^chapter\s*\d+$`)

// creditsChapterPattern matches a title that names the credits from its first
// word, such as "Credits", "End Credits", "Ending Theme", or "Outro: Song".
// Anchoring at the start keeps story chapters like "The Ending" out.
var creditsChapterPattern = regexp.MustCompile(`(?i)^((end|ending|closing)\s+credits|credits|ending|outro|end\s+titles)(\s|:|-|$)`)

// creditsSceneChapterPattern matches a scene that plays around the credits,
// such as "Post-Credits Scene", "After Credits", "Credits Scene", or
// "Mid-credits Stinger". Skipping the credits must not skip it.
var creditsSceneChapterPattern = regexp.MustCompile(`(?i)\b(post|after|mid)[\s-]*credits?\b|\b(scene|stinger|tag|bonus)\b`)

// Chapter credits bounds. The upper bound admits long drama endings whose
// credits chapter also holds the theme song.
const (
	minimumChapterCreditsSeconds = 10
	maximumChapterCreditsSeconds = 600
	// maximumAmbiguousCreditsChapterSeconds bounds a chapter titled only
	// "Ending" or "Outro". Anime endings run about 90 seconds; a longer
	// chapter with that title is usually the story's final scene.
	maximumAmbiguousCreditsChapterSeconds = 180
)

// ambiguousCreditsChapterPattern matches credits titles that can also name a
// story chapter.
var ambiguousCreditsChapterPattern = regexp.MustCompile(`(?i)^(ending|outro)\b`)

func DetectChapterIntro(chapters []models.MediaChapter) (Segment, bool) {
	for i, chapter := range chapters {
		title := strings.TrimSpace(chapter.Title)
		if !isIntroChapterTitle(title) {
			continue
		}
		if i > 0 && isIntroChapterTitle(strings.TrimSpace(chapters[i-1].Title)) {
			continue
		}
		if i+1 < len(chapters) && isIntroChapterTitle(strings.TrimSpace(chapters[i+1].Title)) {
			continue
		}

		end := chapter.EndSeconds
		if i+1 < len(chapters) && chapters[i+1].StartSeconds > chapter.StartSeconds {
			end = chapters[i+1].StartSeconds
		}
		duration := end - chapter.StartSeconds
		if duration < 10 || duration > 180 {
			continue
		}
		return Segment{
			Start:      chapter.StartSeconds,
			End:        end,
			Confidence: 0.95,
			Algorithm:  ChapterAlgorithm,
		}, true
	}
	return Segment{}, false
}

// DetectChapterCredits returns the last run of credits chapters in the second
// half of the file. Adjacent credits chapters, such as "Ending" followed by
// "End Credits", form one segment. The credits end where the next chapter
// starts, so a following preview or post-credits scene is not skipped.
func DetectChapterCredits(chapters []models.MediaChapter, durationSeconds float64) (Segment, bool) {
	isCredits := func(i int) bool {
		title := strings.TrimSpace(chapters[i].Title)
		if !isCreditsChapterTitle(title) {
			return false
		}
		if !ambiguousCreditsChapterPattern.MatchString(title) || strings.Contains(strings.ToLower(title), "credits") {
			return true
		}
		return chapterLength(chapters, i, durationSeconds) <= maximumAmbiguousCreditsChapterSeconds
	}
	last := -1
	for i := len(chapters) - 1; i >= 0; i-- {
		if isCredits(i) {
			last = i
			break
		}
	}
	if last < 0 {
		return Segment{}, false
	}
	first := last
	for first > 0 && isCredits(first-1) {
		first--
	}

	start := chapters[first].StartSeconds
	end := chapters[last].EndSeconds
	if last+1 < len(chapters) && chapters[last+1].StartSeconds > start {
		end = chapters[last+1].StartSeconds
	} else if end <= start && durationSeconds > 0 {
		end = durationSeconds
	}
	if durationSeconds > 0 {
		end = min(end, durationSeconds)
		if start < durationSeconds/2 {
			return Segment{}, false
		}
	}
	duration := end - start
	if duration < minimumChapterCreditsSeconds || duration > maximumChapterCreditsSeconds {
		return Segment{}, false
	}
	return Segment{
		Start:      start,
		End:        end,
		Confidence: 0.95,
		Algorithm:  CreditsChapterAlgorithm,
	}, true
}

// chapterLength is how long chapter i plays: until the next chapter starts, or
// until its own end or the end of the file.
func chapterLength(chapters []models.MediaChapter, i int, durationSeconds float64) float64 {
	start := chapters[i].StartSeconds
	end := chapters[i].EndSeconds
	if i+1 < len(chapters) && chapters[i+1].StartSeconds > start {
		end = chapters[i+1].StartSeconds
	} else if end <= start && durationSeconds > 0 {
		end = durationSeconds
	}
	return end - start
}

// isCreditsChapterTitle matches closing credits. "Opening Credits" is an intro.
func isCreditsChapterTitle(title string) bool {
	if title == "" || generatedChapterPattern.MatchString(title) || isIntroChapterTitle(title) ||
		creditsSceneChapterPattern.MatchString(title) {
		return false
	}
	return creditsChapterPattern.MatchString(title) || isExplicitTagChapterTitle(title, "ED")
}

func isIntroChapterTitle(title string) bool {
	if title == "" || generatedChapterPattern.MatchString(title) {
		return false
	}
	if introChapterPattern.MatchString(title) {
		return true
	}
	return isExplicitTagChapterTitle(title, "OP")
}

// isExplicitTagChapterTitle matches an upper-case anime chapter tag such as
// "OP", "ED2", or "ED: Ending". Lower-case forms are ambiguous ("Op. 5").
func isExplicitTagChapterTitle(title, tag string) bool {
	if !strings.HasPrefix(title, tag) {
		return false
	}
	if len(title) == len(tag) {
		return true
	}
	switch next := title[len(tag)]; {
	case next >= '0' && next <= '9':
		return true
	case next == ' ' || next == ':' || next == '-':
		return strings.TrimSpace(title[len(tag)+1:]) != ""
	default:
		return false
	}
}
