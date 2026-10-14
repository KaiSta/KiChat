package main

import (
	"encoding/json"
	"strings"
	"testing"

	"kichat/internal/config"
)

// TestAnImageProfileSurvivesASave is the whole point of the feature: a
// configuration named in the panel comes back after a restart, so "switch to my
// Qwen setup" is one pick rather than a form filled in from memory.
func TestAnImageProfileSurvivesASave(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	draft := readSettings(t, a)
	draft.Images.Enabled = true
	draft.ImageProfiles = []config.ImageProfile{{
		Name: "Qwen Image (CLI)",
		Settings: config.ImageSettings{
			Kind:              config.ImageKindCLI,
			CLIFolder:         "/home/user/Downloads/sd",
			CLIDiffusionModel: "/models/qwen-image-2.1-Q5_K_M.gguf",
			CLIVAE:            "/models/qwen_image_vae.safetensors",
			CLIOffloadToCPU:   true,
			CLIExtraArgs:      "--diffusion-fa",
			Sampler:           "euler",
			Steps:             8,
			// The one generation field that is not a whole number, so it is the one a
			// profile could lose to a rounding or a zero-value check.
			CfgScale:       2.5,
			Width:          1024,
			Height:         1024,
			NegativePrompt: "blurry",
		},
	}}
	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// The row is where a profile has to live: it is a configuration, not a
	// session-only convenience, and nothing else in the app writes it.
	var blob config.Settings
	found, err := a.store.GetSetting("settings", &blob)
	if err != nil || !found {
		t.Fatalf("read the settings row: found=%v err=%v", found, err)
	}
	if len(blob.ImageProfiles) != 1 {
		t.Fatalf("the row holds %d profiles, want the saved one", len(blob.ImageProfiles))
	}
	if got := blob.ImageProfiles[0].Name; got != "Qwen Image (CLI)" {
		t.Errorf("profile name = %q, want the typed one", got)
	}
	if got := blob.ImageProfiles[0].Settings.CLIDiffusionModel; got != "/models/qwen-image-2.1-Q5_K_M.gguf" {
		t.Errorf("diffusion model = %q, want the saved path", got)
	}
	if !blob.ImageProfiles[0].Settings.CLIOffloadToCPU {
		t.Error("the offload switch did not survive the save")
	}
	if got := blob.ImageProfiles[0].Settings.CfgScale; got != 2.5 {
		t.Errorf("cfg scale = %v, want the saved 2.5 — a profile that loses it applies a different scale than it shows", got)
	}

	// And the panel reads them back through Load, which is where a fresh start
	// gets them from.
	loaded, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(loaded.ImageProfiles) != 1 {
		t.Fatalf("a reload found %d profiles, want 1", len(loaded.ImageProfiles))
	}
	got := loaded.ImageProfiles[0].Settings
	if got.Kind != config.ImageKindCLI || got.Sampler != "euler" || got.Steps != 8 || got.Width != 1024 {
		t.Errorf("the reloaded profile lost fields: %+v", got)
	}
	if got.NegativePrompt != "blurry" {
		t.Errorf("negative prompt = %q, want the saved one", got.NegativePrompt)
	}
	// The feature switch is not part of a profile — applying one must not be able
	// to switch image generation off — which is also why the saved copy keeps the
	// switch where the user put it.
	if !loaded.Images.Enabled {
		t.Error("the feature switch did not survive the save")
	}
}

// TestAnImageProfileCannotCarryACredential is the secret rule applied to the new
// shape. A profile holds a whole ImageSettings, so without this the "user:pass" a
// user pasted would be written into the settings row the moment they saved the
// same configuration as a profile — the one thing the sanitising pass exists to
// prevent, and invisible in the app because Load shadows the field.
func TestAnImageProfileCannotCarryACredential(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	draft := readSettings(t, a)
	draft.ImageProfiles = []config.ImageProfile{{
		Name: "Remote WebUI",
		Settings: config.ImageSettings{
			BaseURL: "http://10.0.0.5:7860",
			APIKey:  "user:secretpass",
		},
	}}
	stored, err := a.SaveSettings(draft)
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Neither half of the boundary may carry it: not the row on disk...
	var blob config.Settings
	if found, err := a.store.GetSetting("settings", &blob); err != nil || !found {
		t.Fatalf("read the settings row: found=%v err=%v", found, err)
	}
	if len(blob.ImageProfiles) != 1 {
		t.Fatalf("the row holds %d profiles, want the saved one", len(blob.ImageProfiles))
	}
	if blob.ImageProfiles[0].Settings.APIKey != "" {
		t.Error("the settings row carries a profile credential; secrets live in the secret store")
	}
	// ...nor the copy the bridge hands back.
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "secretpass") {
		t.Error("a profile credential crossed the bridge")
	}
	// And the app must not have stored it under the feature's own name either: a
	// profile's key is not the server's credential, and writing it there would
	// authenticate every other profile with it.
	if v, ok := a.secrets.Get(config.ImageAPIKeyName); ok {
		t.Errorf("a profile credential was stored as the feature's: %q", v)
	}
	// The address is not a secret, so it is kept.
	if blob.ImageProfiles[0].Settings.BaseURL != "http://10.0.0.5:7860" {
		t.Errorf("profile address = %q, want the saved one", blob.ImageProfiles[0].Settings.BaseURL)
	}
}

// TestNormaliseRepairsImageProfiles covers the states only a hand-edited or
// older blob can be in. Every one of them is a failure the panel would otherwise
// carry into a draw: a nameless profile can never be selected, a repeated name
// leaves "update" with two candidates, and an out-of-bounds size would re-arm the
// request the clamp on the live fields exists to prevent — once per pick, which is
// the opposite of what a saved configuration is for.
func TestNormaliseRepairsImageProfiles(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	blob := config.Default()
	blob.ImageProfiles = []config.ImageProfile{
		{Name: "  SDXL  ", Settings: config.ImageSettings{Model: "sdxl.safetensors"}},
		{Name: "   ", Settings: config.ImageSettings{Model: "nameless.safetensors"}},
		{Name: "SDXL", Settings: config.ImageSettings{Model: "second.safetensors"}},
		{Name: "Too big", Settings: config.ImageSettings{
			Width: 20000, Height: 4, Steps: 9000, CfgScale: 250, APIKey: "user:secretpass",
		}},
	}
	if err := a.store.PutSetting("settings", blob); err != nil {
		t.Fatalf("write the row: %v", err)
	}

	loaded, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(loaded.ImageProfiles) != 2 {
		t.Fatalf("loaded %d profiles, want 2 (the unnamed and the duplicate dropped): %+v",
			len(loaded.ImageProfiles), loaded.ImageProfiles)
	}
	// The trimmed name, and the *first* of the two that shared it: a repair must not
	// silently hand the second one's configuration to the first one's name.
	if got := loaded.ImageProfiles[0]; got.Name != "SDXL" || got.Settings.Model != "sdxl.safetensors" {
		t.Errorf("first profile = %+v, want the trimmed first SDXL", got)
	}
	big := loaded.ImageProfiles[1]
	if big.Name != "Too big" {
		t.Fatalf("second profile name = %q, want %q", big.Name, "Too big")
	}
	if big.Settings.Width != config.MaxImageDimension || big.Settings.Steps != config.MaxImageSteps {
		t.Errorf("clamps: width=%d steps=%d, want %d and %d",
			big.Settings.Width, big.Settings.Steps, config.MaxImageDimension, config.MaxImageSteps)
	}
	// A size below the minimum is raised rather than left as a 4-pixel picture.
	if big.Settings.Height != config.MinImageDimension {
		t.Errorf("height = %d, want the minimum %d", big.Settings.Height, config.MinImageDimension)
	}
	// The guidance scale is clamped on this path too, and it is the one bound that is not an
	// integer: a profile is read straight out of the row, so a hand-edited 250 would otherwise
	// be what every picture a user draws with it is pinned to.
	if big.Settings.CfgScale != config.MaxImageCfgScale {
		t.Errorf("cfg scale = %v, want the cap %v", big.Settings.CfgScale, config.MaxImageCfgScale)
	}
	if big.Settings.APIKey != "" {
		t.Error("a profile from the row kept a credential")
	}
}
