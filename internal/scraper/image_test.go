package scraper

import (
	"testing"
)

func TestCanonicalImageTypeKnownTypes(t *testing.T) {
	for _, imageType := range []string{"poster", "backdrop", "logo", "disc", "banner", "thumb", "clearart", "landscape"} {
		if got, err := CanonicalImageType(imageType); err != nil || got != imageType {
			t.Fatalf("CanonicalImageType(%q) = %q, %v", imageType, got, err)
		}
	}
}

func TestCanonicalImageTypeNormalizesInput(t *testing.T) {
	cases := map[string]string{
		"  POSTER  ": "poster",
		"FanArt":     "backdrop",
		"background": "backdrop",
		"cdart":      "disc",
		"BACKDROP":   "backdrop",
	}
	for input, want := range cases {
		got, err := CanonicalImageType(input)
		if err != nil {
			t.Fatalf("CanonicalImageType(%q) 报错: %v", input, err)
		}
		if got != want {
			t.Fatalf("CanonicalImageType(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestCanonicalImageTypeRejectsUnknown(t *testing.T) {
	for _, imageType := range []string{"", "posterx", "截屏"} {
		if _, err := CanonicalImageType(imageType); err == nil {
			t.Fatalf("未知图片类型 %q 应当报错", imageType)
		}
	}
}

func TestImageExtensionFromURL(t *testing.T) {
	cases := map[string]string{
		"https://image.tmdb.org/a/poster.jpg":     "jpg",
		"https://image.tmdb.org/a/poster.JPG":     "jpg",
		"https://image.tmdb.org/a/poster.png?x=1": "png",
		"https://image.tmdb.org/a/backdrop.webp":  "webp",
		"https://image.tmdb.org/a/backdrop.jpeg":  "jpeg",
	}
	for raw, want := range cases {
		if got := imageExtension(raw); got != want {
			t.Fatalf("imageExtension(%q) = %q，期望 %q", raw, got, want)
		}
	}
}
