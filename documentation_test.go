package sam

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestDocumentationBundleRejectsUnsafeResourcePaths(t *testing.T) {
	for name, resource := range map[string]*fstest.MapFile{
		"../escape": {Data: []byte("outside")},
		"docs/link": {Data: []byte("target"), Mode: fs.ModeSymlink},
	} {
		if _, _, err := collectDocumentation(fstest.MapFS{name: resource}); err == nil {
			t.Errorf("accepted unsafe embedded resource %q", name)
		}
	}
}
