package backup

import (
	"archive/tar"
	"bytes"
	"testing"
)

// Archive entries must not escape the destination directory.
func TestExtractRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/evil", "a/../../evil", "ok/../../../evil"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		if err := extract(tar.NewReader(&buf), t.TempDir()); err == nil {
			t.Errorf("%q extracted", name)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc", Typeflag: tar.TypeSymlink})
	tw.Close()
	if err := extract(tar.NewReader(&buf), t.TempDir()); err == nil {
		t.Error("symlink extracted")
	}
}
