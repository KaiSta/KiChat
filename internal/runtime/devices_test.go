package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two shapes below are the real `--list-devices` output measured on b11538:
// the Vulkan build against a Radeon RX 7900 XT with Mesa's RADV driver, and the
// CPU build on the same machine. They are kept verbatim — trimming them to the
// part that matters would let the parser drift away from what llama.cpp prints.
const (
	vulkanDeviceList = "Available devices:\n" +
		"  Vulkan0: AMD Radeon RX 7900 XT (RADV NAVI31) (20464 MiB, 18464 MiB free)\n"
	cpuDeviceList = "Available devices:\n" +
		"  (none)\n"
)

func TestParseDeviceListReadsWhatLlamaCppPrints(t *testing.T) {
	devices, err := parseDeviceList(vulkanDeviceList)
	if err != nil {
		t.Fatalf("parseDeviceList: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want the one Vulkan device", devices)
	}
	d := devices[0]
	if d.ID != "Vulkan0" {
		t.Errorf("id = %q, want Vulkan0", d.ID)
	}
	if !strings.Contains(d.Raw, "RX 7900 XT") || !strings.Contains(d.Raw, "RADV NAVI31") {
		t.Errorf("raw = %q, want the untouched line for the panel to show", d.Raw)
	}
	// The memory is the reason this parse exists: it is what decides whether a
	// model can be placed on the device at all.
	if !d.Memory || d.TotalMiB != 20464 || d.FreeMiB != 18464 {
		t.Errorf("memory = %d/%d (known=%v), want 20464 total and 18464 free", d.TotalMiB, d.FreeMiB, d.Memory)
	}
	// The card name has parentheses of its own, which is the trap this pins: a
	// parser taking the first bracket group would read the driver name as memory.
	nameAndFree := strings.TrimSuffix(d.Raw, "(20464 MiB, 18464 MiB free)")
	if strings.Contains(nameAndFree, "MiB") {
		t.Errorf("raw = %q, the memory was matched in the wrong parentheses", d.Raw)
	}
}

// The trap this guard exists for: a CPU-only build answers "(none)" rather than an
// empty output, so a parser that only looked for device lines would report the
// machine's own line as a device — or nothing at all, when the whole point is that
// there is no accelerator.
func TestParseDeviceListReportsNoDevicesForNone(t *testing.T) {
	devices, err := parseDeviceList(cpuDeviceList)
	if err != nil {
		t.Fatalf("parseDeviceList: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("devices = %+v, want none for \"(none)\"", devices)
	}
	// Never nil: this feeds a JSON array, and a nil slice marshals to `null`.
	if devices == nil {
		t.Error("devices is nil, want an empty slice")
	}
}

// A device named without its memory still counts as a device. Reporting it as
// unreadable would hide a GPU, and reporting free memory of zero would make every
// model look too large for it.
func TestParseDeviceEntryKeepsADeviceWithoutMemory(t *testing.T) {
	d := parseDeviceEntry("CUDA0: NVIDIA GeForce RTX 4090")
	if d.ID != "CUDA0" || d.Memory {
		t.Errorf("device = %+v, want the id kept and the memory marked unknown", d)
	}
	if d.TotalMiB != 0 || d.FreeMiB != 0 {
		t.Errorf("memory = %d/%d, want zeros when none was reported", d.TotalMiB, d.FreeMiB)
	}
}

// A launcher that prints something else — an older build without the flag, or one
// that died — must be reported as unreadable rather than as having no GPU, because
// the two mean very different things to a user.
func TestParseDeviceListDistinguishesUnreadable(t *testing.T) {
	if _, err := parseDeviceList("error: unrecognized argument: --list-devices\n"); err == nil {
		t.Error("want an error for output with no device list in it")
	}
}

// The sum is what a model is measured against, because llama.cpp splits a model
// across devices: two cards of 8 GiB really can hold a 12 GiB model between them.
func TestFreeDeviceMemoryAddsUpAcrossDevices(t *testing.T) {
	devices := []Device{{FreeMiB: 8192, Memory: true}, {FreeMiB: 8192, Memory: true}}
	if got := FreeDeviceMemoryMiB(devices); got != 16384 {
		t.Errorf("free = %d MiB, want the two devices added up", got)
	}
	// A device with no memory figure contributes nothing rather than counting as
	// empty, which would make the total look smaller than it is.
	if got := FreeDeviceMemoryMiB([]Device{{Memory: true, FreeMiB: 4096}, {ID: "CUDA0"}}); got != 4096 {
		t.Errorf("free = %d MiB, want only the reported memory counted", got)
	}
	if got := FreeDeviceMemoryMiB(nil); got != 0 {
		t.Errorf("free = %d MiB for no devices, want 0", got)
	}
}

// TestBinaryDevicesRunsTheLauncher: the probe has to pass `--list-devices` and
// collect its stdout, which a stub launcher lets us check without a GPU.
func TestBinaryDevicesRunsTheLauncher(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "llama-server")
	body := "#!/bin/sh\n[ \"$1\" = \"--list-devices\" ] || exit 3\n" +
		"printf 'Available devices:\\n  Test0: A Cards (1 MiB, 1 MiB free)\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	devices, err := BinaryDevices(context.Background(), script)
	if err != nil {
		t.Fatalf("BinaryDevices: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != "Test0" {
		t.Errorf("devices = %+v, want the device the stub printed", devices)
	}
}

// A launcher that cannot run is a readable failure, not a silent "no GPU".
func TestBinaryDevicesReportsAFailingLauncher(t *testing.T) {
	if _, err := BinaryDevices(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("want an error for a launcher that does not exist")
	}
}
