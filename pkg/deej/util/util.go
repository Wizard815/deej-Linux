package util

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"go.uber.org/zap"
)

// EnsureDirExists creates the given directory path if it doesn't already exist
func EnsureDirExists(path string) error {
	if err := os.MkdirAll(path, os.ModePerm); err != nil {
		return fmt.Errorf("ensure directory exists (%s): %w", path, err)
	}

	return nil
}

// FileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// Linux returns true if we're running on Linux
func Linux() bool {
	return runtime.GOOS == "linux"
}

// SetupCloseHandler creates a 'listener' on a new goroutine which will notify the
// program if it receives an interrupt from the OS
func SetupCloseHandler() chan os.Signal {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	return c
}

// GetCurrentWindowProcessNames returns the process names (including extension, if applicable)
// of the current foreground window. This includes child processes belonging to the window.
// On Linux, this supports X11 (and XWayland), Hyprland, Sway, niri, KDE Plasma and GNOME (with an extension)
func GetCurrentWindowProcessNames() ([]string, error) {
	currentWindowLock.Lock()
	defer currentWindowLock.Unlock()

	names, err := getCurrentWindowProcessNames()

	// hand out a copy, since the result is cached and callers are free to modify what they get
	if names != nil {
		names = append([]string{}, names...)
	}

	return names, err
}

// FocusSetupError means that the focused window can't be found until the user installs or enables something
type FocusSetupError struct {
	Message string
}

func (e *FocusSetupError) Error() string {
	return e.Message
}

// guards the cached state kept by getCurrentWindowProcessNames, since both the session map and the slider editor use it
var currentWindowLock sync.Mutex

// SerialPort describes a serial port that might have a deej board connected to it
type SerialPort struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

// AutoSerialPort is the special com_port value that makes deej pick the first likely serial port on its own
const AutoSerialPort = "auto"

// IsAutoSerialPort returns true if the given port name asks for automatic serial port detection
func IsAutoSerialPort(port string) bool {
	port = strings.TrimSpace(port)
	return port == "" || strings.EqualFold(port, AutoSerialPort)
}

// FindSerialPorts lists the serial ports currently present, most likely deej boards first
func FindSerialPorts() ([]SerialPort, error) {
	return findSerialPorts()
}

// SerialPortPermissionHint explains how to fix a "permission denied" error when opening a serial port
func SerialPortPermissionHint() string {
	return serialPortPermissionHint()
}

// OpenExternal spawns a detached window with the provided command and argument
func OpenExternal(logger *zap.SugaredLogger, cmd string, arg string) error {

	// use cmd for windows, bash for linux
	execCommandArgs := []string{"cmd.exe", "/C", "start", "/b", cmd, arg}
	if Linux() {
		execCommandArgs = []string{"/bin/bash", "-c", fmt.Sprintf("%s %s", cmd, arg)}
	}

	command := exec.Command(execCommandArgs[0], execCommandArgs[1:]...)

	if err := command.Run(); err != nil {
		logger.Warnw("Failed to spawn detached process",
			"command", cmd,
			"argument", arg,
			"error", err)

		return fmt.Errorf("spawn detached proc: %w", err)
	}

	return nil
}

// OpenURL opens the given URL in the default browser, without waiting for it to close
func OpenURL(logger *zap.SugaredLogger, url string) error {
	command := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	if Linux() {
		command = exec.Command("xdg-open", url)
	}

	return startDetached(logger, command)
}

// OpenInTextEditor opens the given file for editing: with notepad on windows, or with the default application
// for its type on linux (falling back to a few common text editors). This blocks for a short while on linux
func OpenInTextEditor(logger *zap.SugaredLogger, path string) error {
	if !Linux() {
		return OpenExternal(logger, "notepad.exe", path)
	}

	// xdg-open returns as soon as it launched the right application, and fails if there's none to launch
	if err := exec.Command("xdg-open", path).Run(); err == nil {
		return nil
	} else {
		logger.Debugw("Failed to open file with xdg-open, looking for a text editor", "path", path, "error", err)
	}

	for _, editor := range []string{"gnome-text-editor", "gedit", "kate", "kwrite", "mousepad", "xed", "pluma", "featherpad"} {
		if _, err := exec.LookPath(editor); err == nil {
			return startDetached(logger, exec.Command(editor, path))
		}
	}

	return fmt.Errorf("no text editor found to open %s", path)
}

func startDetached(logger *zap.SugaredLogger, command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		logger.Warnw("Failed to spawn detached process", "command", command.Args, "error", err)
		return fmt.Errorf("spawn detached proc: %w", err)
	}

	// reap the process once it exits, so it doesn't linger around as a zombie
	go command.Wait()

	return nil
}

// NormalizeScalar "trims" the given float32 to 2 points of precision (e.g. 0.15442 -> 0.15)
// This is used both for windows core audio volume levels and for cleaning up slider level values from serial
func NormalizeScalar(v float32) float32 {
	return float32(math.Floor(float64(v)*100) / 100.0)
}

// SignificantlyDifferent returns true if there's a significant enough volume difference between two given values
func SignificantlyDifferent(old float32, new float32, noiseReductionLevel string) bool {

	const (
		noiseReductionHigh = "high"
		noiseReductionLow  = "low"
	)

	// this threshold is solely responsible for dealing with hardware interference when
	// sliders are producing noisy values. this value should be a median value between two
	// round percent values. for instance, 0.025 means volume can move at 3% increments
	var significantDifferenceThreshold float64

	// choose our noise reduction level based on the config-provided value
	switch noiseReductionLevel {
	case noiseReductionHigh:
		significantDifferenceThreshold = 0.035
		break
	case noiseReductionLow:
		significantDifferenceThreshold = 0.015
		break
	default:
		significantDifferenceThreshold = 0.025
		break
	}

	if math.Abs(float64(old-new)) >= significantDifferenceThreshold {
		return true
	}

	// special behavior is needed around the edges of 0.0 and 1.0 - this makes it snap (just a tiny bit) to them
	if (almostEquals(new, 1.0) && old != 1.0) || (almostEquals(new, 0.0) && old != 0.0) {
		return true
	}

	// values are close enough to not warrant any action
	return false
}

// a helper to make sure volume snaps correctly to 0 and 100, where appropriate
func almostEquals(a float32, b float32) bool {
	return math.Abs(float64(a-b)) < 0.000001
}
