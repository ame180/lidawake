package install

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	plist, err := renderPlist(501)
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(bytes.NewReader(plist))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
	}
	for _, want := range []string{label, binPath, logPath, "<string>-allow-uid</string>", "<string>501</string>"} {
		if !bytes.Contains(plist, []byte(want)) {
			t.Errorf("plist missing %q", want)
		}
	}
}
