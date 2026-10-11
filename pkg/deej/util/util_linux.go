package util

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	getCurrentWindowInternalCooldown = time.Millisecond * 350
)

var (
	lastGetCurrentWindowResult []string
	lastGetCurrentWindowError  error
	lastGetCurrentWindowCall   = time.Now().Add(-getCurrentWindowInternalCooldown)

	// matches the USB-to-serial chips and vendors commonly found on arduino and ESP boards
	likelyBoardPattern = regexp.MustCompile(`(?i)arduino|espressif|esp32|esp8266|cp210|ch34|ch910|wch|ftdi|ft232|silicon.?labs|usb.?serial|uart|jtag`)

	// /dev/serial/by-id names look like "usb-Silicon_Labs_CP2102_USB_to_UART_Bridge_Controller_0001-if00-port0"
	serialByIDPrefix = regexp.MustCompile(`^usb-`)
	serialByIDSuffix = regexp.MustCompile(`-if\d+(-port\d+)?$`)
)

func findSerialPorts() ([]SerialPort, error) {
	ports := []SerialPort{}
	seen := map[string]bool{}

	// prefer the stable /dev/serial/by-id names, which survive re-plugging the board and describe what's behind them
	byID, _ := filepath.Glob("/dev/serial/by-id/*")

	sort.SliceStable(byID, func(i, j int) bool {
		return likelyBoardPattern.MatchString(byID[i]) && !likelyBoardPattern.MatchString(byID[j])
	})

	for _, link := range byID {
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}

		seen[target] = true

		description := serialByIDSuffix.ReplaceAllString(serialByIDPrefix.ReplaceAllString(filepath.Base(link), ""), "")
		description = strings.ReplaceAll(description, "_", " ")

		ports = append(ports, SerialPort{
			Path:        link,
			Description: description + " (" + filepath.Base(target) + ")",
		})
	}

	// then any other USB serial devices that udev didn't give a name to.
	// ESP32-S2/S3/C3 boards with native USB show up as ttyACM, as do most arduinos.
	// boards with a separate USB-to-serial chip (CP2102, CH340) show up as ttyUSB
	for _, pattern := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)

		for _, match := range matches {
			if seen[match] {
				continue
			}

			seen[match] = true
			ports = append(ports, SerialPort{Path: match, Description: filepath.Base(match)})
		}
	}

	return ports, nil
}

// serialPortPermissionHint explains how to fix a "permission denied" error when opening a serial port
func serialPortPermissionHint() string {
	group := "dialout"

	// arch-based distros use the uucp group instead
	if _, err := os.Stat("/etc/arch-release"); err == nil {
		group = "uucp"
	}

	return "Add yourself to the " + group + " group (sudo usermod -aG " + group + " $USER), then log out and back in."
}
