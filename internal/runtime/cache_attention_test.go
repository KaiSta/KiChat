package runtime

import (
	"testing"

	"kichat/internal/uimsg"
)

// A quantized V cache is implemented through flash attention, and llama.cpp refuses to
// load one with flash attention off: "quantized V cache requires flash_attn to be
// enabled", measured on build 12189. The panel lets both be chosen, so the start has to
// refuse the pair as a sentence about the settings rather than let the server fail.
func TestAQuantizedVCacheWithoutFlashAttentionIsRefusedBeforeTheServerRuns(t *testing.T) {
	for _, ct := range []string{"q8_0", "q5_1", "q4_0", "iq4_nl"} {
		err := Options{Model: "/m.gguf", FlashAttention: FlashAttentionOff, CacheTypeV: ct}.cacheAttentionRefusal()
		if err == nil {
			t.Errorf("a %s V cache with flash attention off was accepted", ct)
			continue
		}
		msg, ok := uimsg.Parse(err.Error())
		if !ok || msg.Code != "runtime.quantizedVCacheNeedsFlashAttention" {
			t.Errorf("refusal for %s = %v, want the quantized-V-cache message", ct, err)
		}
	}

	// What has to keep working: the server's own "auto" turns flash attention on for a
	// quantized cache, an explicit on is the mode that makes it legal, and the
	// unquantized types need no attention flag at all.
	for _, o := range []Options{
		{Model: "/m.gguf", FlashAttention: FlashAttentionOff, CacheTypeV: CacheTypeF16},
		{Model: "/m.gguf", FlashAttention: FlashAttentionOff, CacheTypeV: CacheTypeF32},
		{Model: "/m.gguf", FlashAttention: FlashAttentionOff, CacheTypeV: CacheTypeBF16},
		{Model: "/m.gguf", FlashAttention: FlashAttentionOff, CacheTypeV: ""},
		{Model: "/m.gguf", FlashAttention: FlashAttentionOn, CacheTypeV: "q8_0"},
		{Model: "/m.gguf", FlashAttention: "auto", CacheTypeV: "q8_0"},
		{Model: "/m.gguf", CacheTypeV: "q8_0"},
	} {
		if err := o.cacheAttentionRefusal(); err != nil {
			t.Errorf("%+v was refused: %v", o, err)
		}
	}

	// And the wiring: the refusal is the answer a start gives, before it looks at any
	// file.
	err := NewServer("test").Start(Options{FlashAttention: FlashAttentionOff, CacheTypeV: "q8_0"})
	if msg, ok := uimsg.Parse(err.Error()); err == nil || !ok || msg.Code != "runtime.quantizedVCacheNeedsFlashAttention" {
		t.Errorf("start = %v, want the quantized-V-cache refusal", err)
	}
}
