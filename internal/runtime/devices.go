package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Device is one compute device a launcher reported, with the memory it named.
//
// Raw is what llama.cpp printed and is what the panel shows: the id, the card
// name and the free memory are already phrased there, and rebuilding that string
// from fields would only risk printing something the server never said.
type Device struct {
	Raw string
	// ID is the short name the server uses for the device (`Vulkan0`), empty when
	// the line did not have the `<id>: <name>` shape.
	ID string
	// TotalMiB and FreeMiB are the numbers in the entry. They are 0 when the
	// entry did not carry them, which is not an error: a build that names a
	// device without its memory is still a build that has one.
	TotalMiB int
	FreeMiB  int
	// Memory is false when the entry named no memory at all, so a caller can tell
	// "0 MiB free" from "not reported".
	Memory bool
}

// deviceEntry matches one measured `--list-devices` line:
//
//	Vulkan0: AMD Radeon RX 7900 XT (RADV NAVI31) (20464 MiB, 18464 MiB free)
//
// The card name carries its own parentheses (`(RADV NAVI31)`), so the memory group
// is anchored to the end of the line rather than taking the first brackets found.
var deviceEntry = regexp.MustCompile(`^([^:]+):\s*(.*?)\s*(?:\((\d+) MiB,\s*(\d+) MiB free\))?$`)

// BinaryDevices asks a launcher which compute devices it can use.
//
// Why a probe and not the running server's log: at its default verbosity the
// server prints nothing about devices at all — measured on b11538, a load of a
// small model produced eleven log lines and not one of them named the device,
// while the same load at `-lv 4` printed the full `device_info:` block. Reaching
// the device list through the log would therefore mean starting every managed
// model with a talking flag, and a flag an older custom binary does not know would
// break the server rather than a report. `--list-devices` answers the same question
// in a process that is allowed to fail: whatever it cannot tell stays unknown.
//
// An empty list means the launcher sees no accelerator and will run on the CPU,
// which is exactly the silent fallback this exists to make visible — the CPU build
// answers `(none)`, and so does a GPU build whose runtime libraries are missing.
func BinaryDevices(ctx context.Context, binary string) ([]Device, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("kein llama-server angegeben")
	}
	cmd := exec.CommandContext(ctx, binary, "--list-devices")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The environment a server is started with, for the same reason: a HIP build
	// resolves its runtime from the system, and when /opt/rocm/lib is not on the
	// loader path it reports "(none)" — the probe would then be describing this
	// process's environment rather than the build's. See serverEnv.
	cmd.Env = serverEnv(binary)
	// Cancelling the context kills the launcher, but not a child it forked, and a
	// child inherits the pipe this collects through; without this the probe would
	// sit on that pipe. Measured on the version probe, see BinaryVersion.
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()

	devices, err := parseDeviceList(out.String())
	if err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("Geräte von %s lesen: %w", binary, runErr)
		}
		return nil, err
	}
	return devices, nil
}

// parseDeviceList reads the device lines out of `--list-devices` output.
//
// The measured shape on b11538 is a header and one indented line per device, on
// stdout:
//
//	Available devices:
//	  Vulkan0: AMD Radeon RX 7900 XT (RADV NAVI31) (20464 MiB, 18464 MiB free)
//
// and for a build with no accelerator:
//
//	Available devices:
//	  (none)
//
// An unindented line after the header begins something else, so the list has ended
// there.
func parseDeviceList(out string) ([]Device, error) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "Available devices:") {
			continue
		}
		devices := []Device{}
		for _, rest := range lines[i+1:] {
			if strings.TrimSpace(rest) == "" {
				continue
			}
			if rest[0] != ' ' && rest[0] != '\t' {
				break
			}
			entry := strings.TrimSpace(rest)
			if entry == "(none)" {
				return []Device{}, nil
			}
			devices = append(devices, parseDeviceEntry(entry))
		}
		return devices, nil
	}
	return nil, errors.New("keine Geräteliste in der Ausgabe")
}

// parseDeviceEntry splits one device line into its text and its memory. A line
// that does not fit the shape is kept whole with no memory, rather than dropped:
// an unfamiliar device is still a device to show.
func parseDeviceEntry(entry string) Device {
	device := Device{Raw: entry}
	match := deviceEntry.FindStringSubmatch(entry)
	if match == nil {
		return device
	}
	device.ID = strings.TrimSpace(match[1])
	if match[3] == "" {
		// The shape matched but without the memory group; the name is still worth
		// keeping inside Raw, which is all the panel shows.
		return device
	}
	total, err := strconv.Atoi(match[3])
	if err != nil {
		return device
	}
	free, err := strconv.Atoi(match[4])
	if err != nil {
		return device
	}
	device.TotalMiB, device.FreeMiB, device.Memory = total, free, true
	return device
}

// FreeDeviceMemoryMiB adds up what the reported devices say is free.
//
// It is a sum rather than the largest single device because llama.cpp splits a
// model across devices by default, so a model that fits in the total across two
// cards really can be placed on the GPUs. Devices that named no memory contribute
// nothing, which is the honest answer for them.
func FreeDeviceMemoryMiB(devices []Device) int {
	total := 0
	for _, d := range devices {
		total += d.FreeMiB
	}
	return total
}
