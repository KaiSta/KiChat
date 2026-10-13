package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// cachePicture stands in for a drawn PNG. It is deliberately longer than the one the stop
// tests use, because the numbers in this file are byte counts and a picture of one byte would
// make "the size went down by exactly what was deleted" unfalsifiable.
var cachePicture = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x7f, 0x3c, 0x01, 0x02, 0x03, 0x04, 0x05,
}

// fastImageServer is a WebUI that answers every draw immediately. Nothing here is about
// timing: these tests are about what the *store* ends up holding.
func fastImageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sdapi/v1/txt2img" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"images":["%s"],"info":"{\"seed\":4242}"}`,
			base64.StdEncoding.EncodeToString(cachePicture))
	}))
	t.Cleanup(server.Close)
	return server
}

// newPictureCacheApp is an app with image generation switched on against a server that draws
// instantly, and `pictures` pictures already in the transcript — drawn through the real tool
// path, so each one carries the recipe that makes it a picture KiChat drew rather than a file
// the user attached.
func newPictureCacheApp(t *testing.T, pictures int) (*App, string) {
	t.Helper()
	settings := config.Default()
	settings.Images.Enabled = true
	settings.Images.BaseURL = fastImageServer(t).URL

	// The app's lifetime context is deliberately left nil. Every event crosses it, and a
	// Wails event needs the context its own lifecycle hook handed out — with a plain one the
	// runtime calls log.Fatal, which ends the whole test binary. Nothing in this file is
	// about events; the shape of the progress event is pinned in image_progress_test.go.
	a := newSettingsTestApp(t, settings, t.TempDir())
	a.rebuildServices(settings)

	chatID := "chat-cache"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	for i := 0; i < pictures; i++ {
		ctx := tools.WithCallingMessage(t.Context(), fmt.Sprintf("msg-%d", i+1))
		if _, err := a.currentToolService().Execute(ctx, chatID, tools.ToolGenerateImage,
			fmt.Sprintf(`{"prompt":"picture %d"}`, i+1)); err != nil {
			t.Fatalf("generate_image %d: %v", i+1, err)
		}
	}
	return a, chatID
}

// attachPhoto files a picture the *user* attached, in exactly the shape AddAttachment writes:
// an image attachment with a document holding the payload and no recipe. It is the row the
// clear button must never touch, and it is written through the store rather than through the
// picker because a native dialog cannot open in a test.
// cachedBytes is what the panel reports for a number of drawn pictures: the base64-encoded
// payload the store actually holds, which is what a clear frees. It is not the decoded size
// of the PNGs, because the row a clear deletes is the encoded one.
func cachedBytes(pictures int) int64 {
	return int64(pictures) * ((int64(len(cachePicture)) + 2) / 3 * 4)
}

func attachPhoto(t *testing.T, a *App, chatID string) store.Attachment {
	t.Helper()
	now := time.Now()
	document := store.Document{
		ID: "doc-photo", AttachmentID: "att-photo", ChatID: chatID, Title: "photo.png",
		Text: base64.StdEncoding.EncodeToString(cachePicture), CreatedAt: now,
	}
	if err := a.store.CreateDocument(document); err != nil {
		t.Fatalf("create document: %v", err)
	}
	photo := store.Attachment{
		ID: "att-photo", ChatID: chatID, Kind: string(ingest.KindImage), Filename: "photo.png",
		Mime: "image/png", SizeBytes: int64(len(cachePicture)), Placement: store.PlacementInline,
		DocumentID: document.ID, CreatedAt: now,
	}
	if err := a.store.CreateAttachment(photo); err != nil {
		t.Fatalf("create attachment: %v", err)
	}
	return photo
}

// TestTheImageCacheCountsDrawnPicturesAndTheirUnsavedPart is the panel's reading: how much of
// the app's own storage is pictures KiChat drew, and how much of that nothing outside KiChat
// holds a copy of.
//
// The attached photo is the load-bearing part. It shares the table and the kind with a drawn
// picture, and it differs in exactly one way — it has no recipe — which is what the whole
// feature hangs on: deleting it would take a picture out of a conversation to free nothing,
// since the file it came from is still where the user left it.
func TestTheImageCacheCountsDrawnPicturesAndTheirUnsavedPart(t *testing.T) {
	a, chatID := newPictureCacheApp(t, 2)
	attachPhoto(t, a, chatID)

	cache, err := a.GetImageCache()
	if err != nil {
		t.Fatalf("GetImageCache: %v", err)
	}
	if cache.Count != 2 {
		t.Errorf("count = %d, want the 2 pictures KiChat drew (the attached photo is not a cache entry)", cache.Count)
	}
	wantBytes := cachedBytes(2)
	if cache.Bytes != wantBytes {
		t.Errorf("bytes = %d, want %d (the base64 rows the store holds, not the decoded pictures)", cache.Bytes, wantBytes)
	}
	if cache.UnsavedCount != 2 || cache.UnsavedBytes != wantBytes {
		t.Errorf("unsaved = %d pictures / %d bytes, want all of them: nothing was exported",
			cache.UnsavedCount, cache.UnsavedBytes)
	}
	if cache.UnsavedChats != 1 {
		t.Errorf("chats = %d, want 1 — the button's warning is about how many conversations lose a picture", cache.UnsavedChats)
	}
	// The panel iterates nothing here, but the struct still has to marshal without a null
	// in it: a field that arrives as null where a number is expected is the bug this
	// codebase has shipped twice.
	raw, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "null") {
		t.Errorf("the cache status marshals a null: %s", raw)
	}
}

// TestClearingThePictureCacheRemovesOnlyWhatNobodySaved is the whole button: the pictures that
// live only in KiChat's store go, the one its owner has a file of stays, and a picture the
// user attached is not touched at all.
//
// The two of them are asserted separately because they fail differently. Keeping a saved
// picture is the promise the panel makes in words; keeping an attached one is the difference
// between a cache and a delete-my-data button.
func TestClearingThePictureCacheRemovesOnlyWhatNobodySaved(t *testing.T) {
	a, chatID := newPictureCacheApp(t, 2)
	photo := attachPhoto(t, a, chatID)

	drawn, err := a.drawnPictures()
	if err != nil {
		t.Fatalf("drawnPictures: %v", err)
	}
	if len(drawn) != 2 {
		t.Fatalf("drawn pictures = %d, want 2", len(drawn))
	}
	// What "Save as…" records, done here through the store because a native save dialog
	// cannot open in a test. The write side of that is one call in SaveAttachmentAs; this
	// is the reading the panel depends on.
	if err := a.store.SetAttachmentSavedPath(drawn[0].ID, "/home/user/Pictures/picture-1.png"); err != nil {
		t.Fatalf("SetAttachmentSavedPath: %v", err)
	}

	cache, err := a.GetImageCache()
	if err != nil {
		t.Fatalf("GetImageCache: %v", err)
	}
	if cache.Count != 2 || cache.UnsavedCount != 1 {
		t.Errorf("cache = %d pictures with %d unsaved, want 2 and 1", cache.Count, cache.UnsavedCount)
	}
	if cache.UnsavedBytes != cachedBytes(1) {
		t.Errorf("unsaved bytes = %d, want exactly the one picture's %d", cache.UnsavedBytes, cachedBytes(1))
	}

	cleared, err := a.ClearImageCache()
	if err != nil {
		t.Fatalf("ClearImageCache: %v", err)
	}
	if cleared.Removed != 1 || cleared.Bytes != cachedBytes(1) {
		t.Errorf("cleared = %d pictures / %d bytes, want the 1 unsaved one", cleared.Removed, cleared.Bytes)
	}

	// The saved picture is still there and still counted, and now reads as fully saved.
	after, err := a.GetImageCache()
	if err != nil {
		t.Fatalf("GetImageCache after clearing: %v", err)
	}
	if after.Count != 1 || after.UnsavedCount != 0 {
		t.Errorf("after clearing: %d pictures with %d unsaved, want 1 and 0", after.Count, after.UnsavedCount)
	}
	if after.UnsavedChats != 0 {
		t.Errorf("after clearing: %d chats would lose a picture, want 0", after.UnsavedChats)
	}
	remaining, err := a.drawnPictures()
	if err != nil {
		t.Fatalf("drawnPictures: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != drawn[0].ID {
		t.Errorf("the surviving picture is not the one that was saved: %+v", remaining)
	}

	// The attached photo survived, with its payload: deleting from a cache must not be a
	// way to lose an attachment.
	if _, err := a.store.GetAttachment(photo.ID); err != nil {
		t.Errorf("the picture the user attached was deleted by the cache clear: %v", err)
	}
	if _, err := a.store.GetDocument(photo.DocumentID); err != nil {
		t.Errorf("the attached photo's document was deleted: %v", err)
	}
	// And the chat still shows it, which is what the attachments event is for. The saved
	// picture is still listed beside it, and the *unsaved* one is gone from the transcript
	// rather than merely from the totals — that is the whole point of the button.
	attachments, err := a.store.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	ids := map[string]bool{}
	for _, attachment := range attachments {
		ids[attachment.ID] = true
	}
	if !ids[photo.ID] {
		t.Errorf("the attached photo is no longer listed in its chat: %+v", attachments)
	}
	if !ids[drawn[0].ID] {
		t.Errorf("the saved picture is no longer listed in its chat: %+v", attachments)
	}
	if ids[drawn[1].ID] {
		t.Errorf("the cleared picture is still listed in the chat: %+v", attachments)
	}

	// A second press has nothing left to do, and that is not an error: the panel's button
	// gets clicked twice like every other one.
	again, err := a.ClearImageCache()
	if err != nil {
		t.Fatalf("second ClearImageCache: %v", err)
	}
	if again.Removed != 0 {
		t.Errorf("second clear removed %d pictures, want 0", again.Removed)
	}
}

// TestTheImageCacheIsEmptyBeforeAnythingIsDrawn is the rules this codebase learned from the
// two crashes a null has already caused: the panel reads this on a machine that has never
// drawn a picture, which is the state most installations are in.
func TestTheImageCacheIsEmptyBeforeAnythingIsDrawn(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	cache, err := a.GetImageCache()
	if err != nil {
		t.Fatalf("GetImageCache: %v", err)
	}
	if cache.Count != 0 || cache.Bytes != 0 || cache.UnsavedCount != 0 || cache.UnsavedBytes != 0 || cache.UnsavedChats != 0 {
		t.Errorf("a store with no pictures reports %+v, want zeroes", cache)
	}
	// Clearing nothing is not a failure either — an unused feature must not be able to
	// produce an error dialog.
	cleared, err := a.ClearImageCache()
	if err != nil {
		t.Fatalf("ClearImageCache on an empty store: %v", err)
	}
	if cleared.Removed != 0 || cleared.Bytes != 0 {
		t.Errorf("cleared = %+v, want nothing", cleared)
	}
}

// TestTheDrawHooksAreWiredIntoTheToolService guards the wiring itself, which is the failure a
// test of either half would miss: the service reports progress and registers draws through two
// callbacks it is handed at build time, and a rebuild that forgets one of them leaves live
// progress and Stop silently dead while everything else keeps working.
func TestTheDrawHooksAreWiredIntoTheToolService(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	a.rebuildServices(config.Default())

	if a.toolSvc == nil {
		t.Fatal("no tool service was built")
	}
	if a.toolSvc.Draws == nil {
		t.Error("the tool service cannot register a draw, so a draw the model started cannot be stopped")
	}
	if a.toolSvc.Progress == nil {
		t.Error("the tool service cannot report a draw, so a draw the model started reports nothing")
	}
}
