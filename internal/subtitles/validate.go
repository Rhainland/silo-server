package subtitles

import (
	"bytes"
	"encoding/binary"
	"errors"
	"regexp"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrUnreadableSubtitle reports an upload whose bytes hold no subtitle cue the
// declared format can carry: a renamed document, binary data, or plain text.
var ErrUnreadableSubtitle = errors.New("unreadable subtitle file")

var (
	// MicroDVD: {start frame}{end frame}text
	microDVDCue = regexp.MustCompile(`(?m)^\s*\{\d+\}\{\d*\}\s*\S`)
	// SubViewer 2: 00:00:01.00,00:00:04.00 followed by a text line.
	subViewerCue = regexp.MustCompile(`(?m)^\s*\d{1,2}:\d{2}:\d{2}[.,]\d{1,3}\s*,\s*\d{1,2}:\d{2}:\d{2}[.,]\d{1,3}\s*\r?\n[^\r\n]*\S`)
)

// ValidateSubtitleContent checks that data holds at least one subtitle cue in
// format. It only reads the bytes; stored content is never rewritten.
// Text in legacy single-byte encodings parses as-is, and UTF-16 files with a
// byte-order mark are decoded for the check.
func ValidateSubtitleContent(format SubtitleFormat, data []byte) error {
	text := decodeUTF16WithBOM(data)
	var ok bool
	switch normalizeFormat(format) {
	case FormatSRT, FormatVTT, FormatASS, FormatSSA:
		cues, err := ParseCuesForFormat(format, text)
		ok = err == nil && len(cues) > 0
	case FormatSUB:
		ok = microDVDCue.Match(text) || subViewerCue.Match(text)
	}
	if !ok {
		return ErrUnreadableSubtitle
	}
	return nil
}

func decodeUTF16WithBOM(data []byte) []byte {
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		order = binary.LittleEndian
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		order = binary.BigEndian
	default:
		return data
	}
	body := data[2:]
	units := make([]uint16, 0, len(body)/2)
	for i := 0; i+1 < len(body); i += 2 {
		units = append(units, order.Uint16(body[i:]))
	}
	out := make([]byte, 0, len(units))
	for _, r := range utf16.Decode(units) {
		out = utf8.AppendRune(out, r)
	}
	return out
}
