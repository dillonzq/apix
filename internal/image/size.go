package image

import (
	"math"
	"strconv"
	"strings"
)

// Aspect ratios each Google protocol accepts.
var (
	geminiAspects = []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"}
	imagenAspects = []string{"1:1", "3:4", "4:3", "9:16", "16:9"}
)

// ratio parses "W:H" or "WxH" into width / height.
func ratio(s, sep string) (float64, bool) {
	w, h, ok := strings.Cut(strings.TrimSpace(s), sep)
	if !ok {
		return 0, false
	}
	wn, err1 := strconv.ParseFloat(w, 64)
	hn, err2 := strconv.ParseFloat(h, 64)
	if err1 != nil || err2 != nil || wn <= 0 || hn <= 0 {
		return 0, false
	}
	return wn / hn, true
}

// nearestAspect returns the aspect ratio in set closest to r on a log scale
// and whether it matches r.
func nearestAspect(r float64, set []string) (string, bool) {
	best, distance := "", math.Inf(1)
	for _, a := range set {
		ar, _ := ratio(a, ":")
		if d := math.Abs(math.Log(r / ar)); d < distance {
			best, distance = a, d
		}
	}
	return best, distance < 0.01
}

// pixelSize parses an OpenAI "WxH" size.
func pixelSize(size string) (w, h int, ok bool) {
	ws, hs, found := strings.Cut(size, "x")
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	return w, h, found && err1 == nil && err2 == nil && w > 0 && h > 0
}

// googleSize maps an OpenAI size onto a Google aspect ratio and image size.
// "auto" and an empty size map to nothing. The image size is the nearest of
// 1K, 2K and 4K by pixel count; 1K, the default, is left empty. exact
// reports whether the aspect ratio is preserved; known reports whether the
// size could be read at all.
func googleSize(size string, aspects []string, maxImageSize string) (aspect, imageSize string, exact, known bool) {
	if size == "" || size == "auto" {
		return "", "", true, true
	}
	w, h, ok := pixelSize(size)
	if !ok {
		return "", "", false, false
	}
	aspect, exact = nearestAspect(float64(w)/float64(h), aspects)
	// Google's 1K, 2K and 4K outputs hold about 1, 4 and 16 megapixels; the
	// thresholds are the geometric means between them.
	switch mp := float64(w*h) / (1 << 20); {
	case mp > 8:
		imageSize = "4K"
	case mp > 2:
		imageSize = "2K"
	}
	if imageSize == "4K" && maxImageSize == "2K" {
		imageSize = "2K"
	}
	return aspect, imageSize, exact, true
}

// openaiSize maps a Google aspect ratio onto the sizes every GPT image model
// accepts: square, 3:2 landscape or 2:3 portrait. exact reports whether the
// aspect ratio is preserved.
func openaiSize(aspect string) (size string, exact bool) {
	if aspect == "" {
		return "", true
	}
	r, ok := ratio(aspect, ":")
	if !ok {
		return "", false
	}
	switch {
	case math.Abs(math.Log(r)) < 0.01:
		return "1024x1024", true
	case r > 1:
		return "1536x1024", math.Abs(math.Log(r/1.5)) < 0.01
	default:
		return "1024x1536", math.Abs(math.Log(r*1.5)) < 0.01
	}
}

// formatOf returns the OpenAI output_format of a MIME type.
func formatOf(mimeType string) string {
	switch strings.ToLower(mimeType) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/webp":
		return "webp"
	}
	return ""
}

// mimeOf returns the MIME type of an OpenAI output_format; PNG is the default.
func mimeOf(format string) string {
	switch format {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	}
	return "image/png"
}

// extension returns a file name extension for an image MIME type.
func extension(mimeType string) string {
	if f := formatOf(mimeType); f != "" {
		if f == "jpeg" {
			return ".jpg"
		}
		return "." + f
	}
	return ""
}
