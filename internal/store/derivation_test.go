package store

import (
	"testing"
	"time"
)

// TestDerivationRoundTrips pins the column that says where an enlarged picture came
// from. It is a different fact from a generation recipe, and the two must not be
// conflated: a redraw of an upscaled photo would invent a picture, while a redraw of a
// generated one is the point of the recipe.
func TestDerivationRoundTrips(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	source := Attachment{
		ID: "a1", ChatID: "c1", MessageID: "m1", Kind: "image",
		Filename: "cat.png", Mime: "image/png", SizeBytes: 12,
		TokenEstimate: 1024, Placement: PlacementInline, DocumentID: "d1",
		CreatedAt: time.Now(),
	}
	if err := st.CreateAttachment(source); err != nil {
		t.Fatalf("CreateAttachment source: %v", err)
	}

	derivation := &Derivation{
		SourceAttachmentID: "a1",
		SourceWidth:        512,
		SourceHeight:       512,
		Method:             "lanczos",
		Scale:              3.75,
		Sharpen:            0.25,
		DurationMs:         740,
	}
	derived := source
	derived.ID = "a2"
	derived.Filename = "cat-1920.png"
	derived.DocumentID = "d2"
	derived.CreatedAt = time.Now().Add(time.Second)
	derived.Derivation = derivation
	if err := st.CreateAttachment(derived); err != nil {
		t.Fatalf("CreateAttachment derived: %v", err)
	}

	got, err := st.GetAttachment("a2")
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if got.Derivation == nil {
		t.Fatal("the derivation did not survive the write")
	}
	if *got.Derivation != *derivation {
		t.Errorf("derivation came back as %+v, want %+v", *got.Derivation, *derivation)
	}
	if got.Generation != nil {
		t.Errorf("an enlarged picture reports a generation recipe: %+v", got.Generation)
	}

	list, err := st.ListAttachments("c1")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListAttachments = %+v, %v", list, err)
	}
	if list[0].Derivation != nil {
		t.Errorf("the source reports a derivation: %+v", list[0].Derivation)
	}
	if list[1].Derivation == nil || list[1].Derivation.SourceAttachmentID != "a1" {
		t.Errorf("the listed enlargement lost its source: %+v", list[1].Derivation)
	}
}
