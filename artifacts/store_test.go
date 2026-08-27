package artifacts

import (
	"strings"
	"testing"
)

func TestRefForBlob(t *testing.T) {
	text := RefForBlob(Blob{
		Kind: KindText, MIMEType: "text/plain", Name: "out.txt", Data: []byte("one\ntwo\n"),
	})
	if !ValidID(text.ID) || text.Bytes != 8 || text.Lines != 2 || text.Name != "out.txt" {
		t.Fatalf("text ref = %+v", text)
	}

	image := RefForBlob(Blob{Kind: KindImage, Data: []byte("pixels")})
	if image.ImageToken != "[image "+image.ID+"]" {
		t.Fatalf("generated image token = %q", image.ImageToken)
	}
	explicit := RefForBlob(Blob{Kind: KindImage, ImageToken: "[image #7]", Data: []byte("other")})
	if explicit.ImageToken != "[image #7]" {
		t.Fatalf("explicit image token = %q", explicit.ImageToken)
	}
	fromReference := RefForBlob(Blob{Kind: KindImage, Reference: "[image #8]", Data: []byte("third")})
	if fromReference.ImageToken != "[image #8]" {
		t.Fatalf("reference image token = %q", fromReference.ImageToken)
	}
}

func TestValidIDRejectsNonCanonicalValues(t *testing.T) {
	valid := RefForBlob(Blob{Data: []byte("payload")}).ID
	if !ValidID(valid) {
		t.Fatalf("valid ID rejected: %q", valid)
	}
	for _, id := range []string{
		"", "payload", "sha256:short", "sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("z", 64),
	} {
		if ValidID(id) {
			t.Fatalf("invalid ID accepted: %q", id)
		}
	}
}
