package organization

import (
	"testing"
)

func TestRenderRelativeTargetMovieAndTV(t *testing.T) {
	t.Parallel()

	moviePath, err := RenderRelativeTarget(NamingInput{
		SourcePath: "/downloads/Dune.Part.Two.2024.2160p.WEB-DL.H265.mkv",
		MediaType:  "电影",
		Title:      "沙丘2",
		Year:       "2024",
	}, NamingTemplates{})
	if err != nil {
		t.Fatalf("movie RenderRelativeTarget() error = %v", err)
	}
	wantMovie := "沙丘2 (2024)/沙丘2 (2024) - 2160p.mkv"
	if moviePath != wantMovie {
		t.Fatalf("movie path = %q, want %q", moviePath, wantMovie)
	}

	tvPath, err := RenderRelativeTarget(NamingInput{
		SourcePath: "/downloads/Frieren.S01E05.1080p.WEB-DL.mkv",
		MediaType:  "电视剧",
		Title:      "葬送的芙莉莲",
		Year:       "2023",
		Season:     1,
		Episodes:   []int{5},
	}, NamingTemplates{})
	if err != nil {
		t.Fatalf("tv RenderRelativeTarget() error = %v", err)
	}
	wantTV := "葬送的芙莉莲 (2023)/Season 1/葬送的芙莉莲 - S01E05 - 第 5 集.mkv"
	if tvPath != wantTV {
		t.Fatalf("tv path = %q, want %q", tvPath, wantTV)
	}
}

func TestRenderRelativeTargetCustomTemplateAndSanitization(t *testing.T) {
	t.Parallel()

	got, err := RenderRelativeTarget(NamingInput{
		SourcePath: "/downloads/Alien.1979.1080p.mkv",
		MediaType:  "电影",
		Title:      "Alien: Director's Cut?",
		Year:       "1979",
		Category:   "科幻电影",
	}, NamingTemplates{
		Movie: "{{category}}/{{title}} [{{year}}]/{{title}}{{fileExt}}",
	})
	if err != nil {
		t.Fatalf("custom RenderRelativeTarget() error = %v", err)
	}
	want := "科幻电影/Alien Director's Cut [1979]/Alien Director's Cut.mkv"
	if got != want {
		t.Fatalf("custom path = %q, want %q", got, want)
	}

	if _, err := RenderRelativeTarget(NamingInput{
		SourcePath:       "/downloads/a.mkv",
		ExplicitTemplate: "../escape/{{title}}{{fileExt}}",
	}, NamingTemplates{}); err == nil {
		t.Fatal("expected traversal template to be rejected")
	}
}
