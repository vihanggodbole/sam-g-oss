package sam

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Attachment is a file a tool returns for the model to read itself, such as a
// PDF or a picture. It is saved with the call's result and sent to the model
// as part of that result on every later request until compaction summarizes
// it as "[file NAME]".
type Attachment struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"` // application/pdf, image/jpeg, image/png, image/gif or image/webp
	Data      []byte `json:"data"`
}

// maxAttachmentBytes bounds all files of one call together: a provider counts
// every file still in the conversation against each request's size limit.
const maxAttachmentBytes = 10 << 20

var attachmentTypes = append([]string{"application/pdf"}, imageTypes...)

// Attach adds a file to the call's result, for the model to read natively. It
// is kept only when the call succeeds. A model that cannot read pictures or
// files (Model.NoImages) gets a "[file NAME]" note instead.
func (c ToolCall) Attach(a Attachment) error {
	if c.attached == nil {
		return errorf(CodeInvalidArgument, "this call cannot carry files")
	}
	if !slices.Contains(attachmentTypes, a.MediaType) {
		return errorf(CodeInvalidArgument, "%s: media_type must be one of %s", a.Name, strings.Join(attachmentTypes, ", "))
	}
	if len(a.Data) == 0 || http.DetectContentType(a.Data) != a.MediaType {
		return errorf(CodeInvalidArgument, "%s is not a %s file", a.Name, a.MediaType)
	}
	total := len(a.Data)
	for _, f := range *c.attached {
		total += len(f.Data)
	}
	if total > maxAttachmentBytes {
		return errorf(CodeInvalidArgument, "files total %d bytes; at most %d bytes can be attached to one result", total, maxAttachmentBytes)
	}
	*c.attached = append(*c.attached, a)
	return nil
}

func (a Attachment) dataURL() string {
	return "data:" + a.MediaType + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
}

// part is the attachment as a Responses input part.
func (a Attachment) part() outputPart {
	if a.MediaType == "application/pdf" {
		return outputPart{Type: "input_file", Filename: a.Name, FileData: a.dataURL()}
	}
	return outputPart{Type: "input_image", ImageURL: a.dataURL()}
}

// resultOutput is a call's saved output: its text, or its text and files.
func resultOutput(text string, files []Attachment) callOutput {
	if len(files) == 0 {
		return callOutput{text: text}
	}
	var parts []outputPart
	if text != "" { // an input_text part must carry text
		parts = append(parts, outputPart{Type: "input_text", Text: text})
	}
	for _, f := range files {
		parts = append(parts, f.part())
	}
	return callOutput{parts: parts}
}

// outputPart is one part of a tool result that carries files.
type outputPart struct {
	Type     string `json:"type"` // input_text, input_file or input_image
	Text     string `json:"text,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// callOutput is a function_call_output's output: a string, or (when the tool
// attached files) a list of parts. It is saved and replayed in the same shape.
type callOutput struct {
	text  string
	parts []outputPart
}

func (o *callOutput) UnmarshalJSON(b []byte) error {
	*o = callOutput{}
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		return json.Unmarshal(b, &o.parts)
	}
	return json.Unmarshal(b, &o.text)
}

func (o callOutput) MarshalJSON() ([]byte, error) {
	if o.parts == nil {
		return json.Marshal(o.text)
	}
	return json.Marshal(o.parts)
}

// String is the result as text, with a note where each file was.
func (o callOutput) String() string {
	if o.parts == nil {
		return o.text
	}
	var sb strings.Builder
	for _, p := range o.parts {
		switch p.Type {
		case "input_text":
			sb.WriteString(p.Text)
		case "input_file":
			sb.WriteString(" [file " + p.Filename + "]")
		case "input_image":
			sb.WriteString(" [image]")
		}
	}
	return sb.String()
}

// files reports whether the output carries any file.
func (o callOutput) files() bool {
	return slices.ContainsFunc(o.parts, func(p outputPart) bool { return p.Type != "input_text" })
}

// costAdjust is what to add to a saved item's byte size so a file counts as
// its provider cost (about a picture per PDF page), never as its base64 text.
func (o callOutput) costAdjust() int {
	n := 0
	for _, p := range o.parts {
		switch p.Type {
		case "input_image":
			n += imageCostBytes - len(p.ImageURL)
		case "input_file":
			pages := 1
			if _, data, ok := splitDataURL(p.FileData); ok {
				if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
					pages = max(1, pdfPages(raw))
				}
			}
			n += pages*imageCostBytes - len(p.FileData)
		}
	}
	return n
}

// messagesContent is the tool_result content for Messages: the text, or the
// text with each file as a document or image block.
func (o callOutput) messagesContent() any {
	if o.parts == nil {
		return o.text
	}
	blocks := []any{}
	for _, p := range o.parts {
		switch p.Type {
		case "input_text":
			if p.Text != "" {
				blocks = append(blocks, messagesText(p.Text))
			}
		case "input_file":
			if mediaType, data, ok := splitDataURL(p.FileData); ok {
				blocks = append(blocks, map[string]any{"type": "document", "title": p.Filename, "source": map[string]string{"type": "base64", "media_type": mediaType, "data": data}})
			}
		case "input_image":
			if mediaType, data, ok := splitDataURL(p.ImageURL); ok {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": mediaType, "data": data}})
			}
		}
	}
	return blocks
}

var pdfPage = regexp.MustCompile(`/Type\s*/Page[^s]`)

// pdfPages counts a PDF's page objects; compressed object streams can hide
// them, so this is an estimate for context accounting only.
func pdfPages(data []byte) int { return len(pdfPage.FindAllIndex(data, -1)) }
