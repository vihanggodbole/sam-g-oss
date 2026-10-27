package sam

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func testPNG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A photo costs a fixed estimate rather than its base64 size, is named in
// compaction summaries, crosses a model switch, and becomes a note for a
// model that cannot read images.
func TestImagesInHistory(t *testing.T) {
	pic := testPNG(t)
	big := userMessage("look", Image{MediaType: "image/jpeg", Data: bytes.Repeat([]byte{0xff}, 1<<20)})
	if got := estimateTokens(modelInputBytes(rawInput(big)[0])); got > imageCostTokens+100 {
		t.Fatalf("a 1 MiB image was estimated at %d tokens", got)
	}
	h := histItem{kind: kindUser, item: rawInput(big)[0]}
	if got := h.portable(); got != "User: look [image]" {
		t.Fatalf("portable: %q", got)
	}
	only := histItem{kind: kindUser, seq: 1, item: rawInput(userMessage("", Image{"image/png", pic}))[0]}
	crossed, _ := (&contextView{portableThrough: 1}).project(only)
	if !strings.Contains(string(crossed), base64.StdEncoding.EncodeToString(pic)) {
		t.Fatalf("an image did not cross a model switch: %s", crossed)
	}
	if got := withoutImages([]json.RawMessage{crossed}); !strings.Contains(string(got[0]), `"text":"[image]"`) || strings.Contains(string(got[0]), "input_image") {
		t.Fatalf("for a model without vision: %s", got[0])
	}
}

func TestImagesAreCheckedBeforeAcceptance(t *testing.T) {
	pic := testPNG(t)
	for name, images := range map[string][]Image{
		"wrong type":     {{MediaType: "image/jpeg", Data: pic}},
		"not an image":   {{MediaType: "image/png", Data: []byte("hello")}},
		"too many":       {{"image/png", pic}, {"image/png", pic}, {"image/png", pic}, {"image/png", pic}, {"image/png", pic}},
		"unknown format": {{MediaType: "image/tiff", Data: pic}},
	} {
		if ErrorCodeOf(validateImages(images)) != CodeInvalidArgument {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := validateImages([]Image{{"image/png", pic}}); err != nil {
		t.Fatal(err)
	}
}

// The Messages protocol carries a user's image as a base64 image block.
func TestMessagesCarryUserImages(t *testing.T) {
	pic := testPNG(t)
	req := modelRequest{
		model: resolvedModel{Alias: "claude", Model: messagesTestModel(), auth: AuthGitHubCopilot}, instructions: "trusted",
		input: rawInput(userMessage("what is this?", Image{"image/png", pic})), maxTokens: 8192,
	}
	raw, err := messagesBody(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"source":{"data":"` + base64.StdEncoding.EncodeToString(pic) + `","media_type":"image/png","type":"base64"},"type":"image"}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("body: %s", raw)
	}
}
