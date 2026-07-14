package metadata

import "strings"

// CachedImageOriginalPath returns the exact stored original key. The fallback
// keeps older ImageCacher test doubles and implementations source compatible
// while callers migrate away from reconstructing object keys themselves.
func CachedImageOriginalPath(result *CacheImageResult) string {
	if result == nil {
		return ""
	}
	if result.OriginalPath != "" {
		return result.OriginalPath
	}
	if result.BasePath == "" {
		return ""
	}
	if strings.Contains(result.BasePath, "/original.") {
		return result.BasePath
	}
	ext := result.Ext
	if ext == "" {
		ext = ".jpg"
	}
	return strings.TrimRight(result.BasePath, "/") + "/original" + ext
}
