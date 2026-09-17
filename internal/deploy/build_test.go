package deploy

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestStageSkipKeepsQueryDocumentsAndDropsSchemas guards the rule that keeps a
// 3.5 MB Shopify SDL out of the pushed tree. It matters twice over: the pushed
// tree is what the daemon stages releases from, and internal/release has a
// second copy of this list that must agree with it.
func TestStageSkipKeepsQueryDocumentsAndDropsSchemas(t *testing.T) {
	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"/repo/integrations/shopify/schema/shopify/shopify.graphql", true,
			"the pulled schema is editor tooling, not a run-time input"},
		{"/repo/integrations/shopify/schema/salesforce/product2.py", false,
			"the generated Python modules are imported while the integration runs"},
		{"/repo/integrations/shopify/queries/products.graphql", false,
			"a query document is read while the integration runs"},
		{"/repo/integrations/shopify/schema-tools/notes.graphql", false,
			"a directory that merely starts with 'schema' is not the schema"},
		{"/repo/integrations/shopify/.env", true, "secrets never travel"},
		{"/repo/integrations/shopify/main.py", false, "the entrypoint is the point"},
	}

	for _, tc := range cases {
		base := filepath.Base(tc.path)
		fsys := fstest.MapFS{base: &fstest.MapFile{}}
		info, err := fs.Stat(fsys, base)
		if err != nil {
			t.Fatal(err)
		}
		if got := stageSkip(tc.path, fs.FileInfoToDirEntry(info)); got != tc.want {
			t.Errorf("stageSkip(%q) = %v, want %v: %s", tc.path, got, tc.want, tc.why)
		}
	}
}
