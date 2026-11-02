package config

import "testing"

// The MLX section is the one place a value can make mlx_lm.server exit instead
// of merely look odd: `--kv-bits 5` is not a width it accepts. Repair belongs at
// the settings boundary, where a blob from an older version or a hand edit is
// read, and again at save time.
func TestNormaliseMLXRepairsWhatTheServerWouldRefuse(t *testing.T) {
	if got := NormaliseMLX(nil); got != nil {
		t.Errorf("NormaliseMLX(nil) = %+v, want nil", got)
	}

	// An all-default section is no section at all: it must not be written into
	// every llama.cpp entry the panel saves.
	if got := NormaliseMLX(&MLXOptions{}); got != nil {
		t.Errorf("an empty MLX section was kept: %+v", got)
	}

	got := NormaliseMLX(&MLXOptions{
		KVBits:       5, // not 2, 3, 4, 6 or 8
		PrefillStep:  -1,
		MaxTokens:    -2,
		DraftModel:   "  /models/draft  ",
		ChatTemplate: " qwen3 ",
	})
	if got == nil {
		t.Fatal("a section with real values was dropped")
	}
	if got.KVBits != 0 {
		t.Errorf("KVBits = %d, want 0 (the server's unquantised default)", got.KVBits)
	}
	if got.PrefillStep != 0 || got.MaxTokens != 0 {
		t.Errorf("negative sizes = %d/%d, want 0/0", got.PrefillStep, got.MaxTokens)
	}
	if got.DraftModel != "/models/draft" || got.ChatTemplate != "qwen3" {
		t.Errorf("trimming = %q/%q", got.DraftModel, got.ChatTemplate)
	}

	for _, bits := range []int{2, 3, 4, 6, 8} {
		if got := NormaliseMLX(&MLXOptions{KVBits: bits}); got == nil || got.KVBits != bits {
			t.Errorf("KVBits %d was repaired away: %+v", bits, got)
		}
	}
}
