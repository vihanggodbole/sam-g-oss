package sam

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// Image is a picture sent with a prompt, such as a photo or screenshot. Data
// holds the file's bytes (base64 in JSON). It is saved in history with its
// prompt and sent to the model on every later request until compaction
// summarizes it as "[image]".
type Image struct {
	MediaType string `json:"media_type"` // image/jpeg, image/png, image/gif or image/webp
	Data      []byte `json:"data"`
}

const (
	maxImages = 4
	// maxImageBytes bounds all images of one prompt together, so even a single
	// image stays within the Messages limit of 5 MB of base64 per image and
	// the server's default request body limit.
	maxImageBytes = 15 << 18 // 3.75 MiB
	// imageCostTokens approximates what a provider counts for one image, so
	// context estimates never treat an image's base64 text as prompt text.
	imageCostTokens = 1500
	imageCostBytes  = imageCostTokens * 5 / 2 // inverse of estimateTokens
)

var imageTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

func validateImages(images []Image) error {
	if len(images) > maxImages {
		return errorf(CodeInvalidArgument, "at most %d images can be sent with one prompt", maxImages)
	}
	total := 0
	for i, img := range images {
		if !slices.Contains(imageTypes, img.MediaType) {
			return errorf(CodeInvalidArgument, "image %d: media_type must be one of %s", i+1, strings.Join(imageTypes, ", "))
		}
		if len(img.Data) == 0 || http.DetectContentType(img.Data) != img.MediaType {
			return errorf(CodeInvalidArgument, "image %d is not a %s file", i+1, img.MediaType)
		}
		total += len(img.Data)
	}
	if total > maxImageBytes {
		return errorf(CodeInvalidArgument, "images total %d bytes; at most %d bytes can be sent with one prompt. Send smaller or fewer images", total, maxImageBytes)
	}
	return nil
}

func (img Image) dataURL() string {
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// splitDataURL returns the media type and base64 data of a saved image.
func splitDataURL(u string) (mediaType, data string, ok bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", false
	}
	mediaType, data, ok = strings.Cut(rest, ";base64,")
	return mediaType, data, ok
}

// withImageNotes marks where images were when only text can be sent.
func withImageNotes(text string, n int) string {
	notes := strings.Repeat(" [image]", n)
	if text == "" {
		return strings.TrimPrefix(notes, " ")
	}
	return text + notes
}

// withoutImages replaces pictures with [image] notes for a model that
// cannot read them, so a session switched to one keeps working.
func withoutImages(input []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(input))
	for i, raw := range input {
		var item outputItem
		if json.Unmarshal(raw, &item) == nil && item.Type == "message" && item.imageCount() > 0 {
			raw = json.RawMessage(mustJSON(userMessage(withImageNotes(item.text(), item.imageCount()))))
		}
		out[i] = raw
	}
	return out
}

// imageCount counts the image parts of a saved message.
func (it outputItem) imageCount() int {
	n := 0
	for _, c := range it.Content {
		if c.Type == "input_image" {
			n++
		}
	}
	return n
}
