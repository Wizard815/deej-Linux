package deej

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"sort"
	"strings"

	"gopkg.in/yaml.v2"

	"github.com/omriharel/deej/pkg/deej/util"
)

// editableConfig holds the user config's values, as shown and saved by the slider editor
type editableConfig struct {
	SliderMapping  map[int][]string `json:"sliderMapping"`
	InvertSliders  bool             `json:"invertSliders"`
	COMPort        string           `json:"comPort"`
	BaudRate       int              `json:"baudRate"`
	NoiseReduction string           `json:"noiseReduction"`
	SliderMaxValue int              `json:"sliderMaxValue"`
}

const (
	userConfigBackupFilepath = userConfigFilepath + ".bak"

	// a sanity limit, no board has anywhere near this many analog pins
	maxSliderIndex = 63
)

var validNoiseReductionLevels = []string{"low", defaultNoiseReductionLevel, "high"}

func defaultEditableConfig() editableConfig {
	return editableConfig{
		SliderMapping:  map[int][]string{0: {masterSessionName}},
		COMPort:        defaultCOMPort,
		BaudRate:       defaultBaudRate,
		NoiseReduction: defaultNoiseReductionLevel,
		SliderMaxValue: defaultSliderMaxValue,
	}
}

// normalize cleans up values coming from the slider editor, and rejects ones that would make an invalid config
func (c *editableConfig) normalize() error {
	mapping := map[int][]string{}

	for sliderIdx, targets := range c.SliderMapping {
		if sliderIdx < 0 || sliderIdx > maxSliderIndex {
			return fmt.Errorf("slider index %d is out of range (0-%d)", sliderIdx, maxSliderIndex)
		}

		cleanTargets := []string{}
		seen := map[string]bool{}

		for _, target := range targets {
			target = strings.TrimSpace(target)

			if target == "" || seen[strings.ToLower(target)] {
				continue
			}

			seen[strings.ToLower(target)] = true
			cleanTargets = append(cleanTargets, target)
		}

		if len(cleanTargets) > 0 {
			mapping[sliderIdx] = cleanTargets
		}
	}

	c.SliderMapping = mapping

	c.COMPort = strings.TrimSpace(c.COMPort)
	if util.IsAutoSerialPort(c.COMPort) {
		c.COMPort = util.AutoSerialPort
	}

	if c.BaudRate <= 0 {
		return fmt.Errorf("baud rate must be a positive number")
	}

	if c.SliderMaxValue <= 0 {
		return fmt.Errorf("slider max value must be a positive number")
	}

	if c.NoiseReduction == "" {
		c.NoiseReduction = defaultNoiseReductionLevel
	}

	validNoiseReduction := false
	for _, level := range validNoiseReductionLevels {
		if c.NoiseReduction == level {
			validNoiseReduction = true
		}
	}

	if !validNoiseReduction {
		return fmt.Errorf("noise reduction must be one of %s", strings.Join(validNoiseReductionLevels, ", "))
	}

	return nil
}

// writeConfigFile saves the given values to config.yaml, keeping a backup of the previous file.
// the file is overwritten in-place, which triggers deej's usual config reload
func writeConfigFile(c editableConfig) error {
	if err := c.normalize(); err != nil {
		return err
	}

	var previous []byte

	if util.FileExists(userConfigFilepath) {
		var err error

		if previous, err = ioutil.ReadFile(userConfigFilepath); err != nil {
			return fmt.Errorf("read existing config: %w", err)
		}

		if err := ioutil.WriteFile(userConfigBackupFilepath, previous, 0644); err != nil {
			return fmt.Errorf("back up existing config: %w", err)
		}
	}

	contents, err := renderConfigFile(c, previous)
	if err != nil {
		return fmt.Errorf("render config: %w", err)
	}

	if err := ioutil.WriteFile(userConfigFilepath, contents, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

func renderConfigFile(c editableConfig, previous []byte) ([]byte, error) {
	var b bytes.Buffer

	b.WriteString(`# process names are case-insensitive
# you can use 'master' to indicate the master channel, or a list of process names to create a group
# you can use 'mic' to control your mic input level (uses the default recording device)
# you can use 'system' to control system sounds (on linux, these are notification and alert sounds)
# you can use 'deej.current' to control the currently focused app (whether full-screen or not)
# you can use 'deej.unmapped' to control all apps that aren't bound to any slider (this ignores master, system, mic and device-targeting sessions)
# you can use a device's full name, i.e. "Speakers (Realtek High Definition Audio)" on windows or "Built-in Audio Analog Stereo" on linux, to bind it. this works for both output and input devices
# important: slider indexes start at 0, regardless of which analog pins you're using!
# tip: you can also edit all of this with the slider editor (right-click deej's tray icon and choose "Open slider editor")
slider_mapping:
`)

	sliderIndices := make([]int, 0, len(c.SliderMapping))
	for sliderIdx := range c.SliderMapping {
		sliderIndices = append(sliderIndices, sliderIdx)
	}

	sort.Ints(sliderIndices)

	for _, sliderIdx := range sliderIndices {
		targets := c.SliderMapping[sliderIdx]

		if len(targets) == 1 {
			fmt.Fprintf(&b, "  %d: %s\n", sliderIdx, yamlScalar(targets[0]))
			continue
		}

		fmt.Fprintf(&b, "  %d:\n", sliderIdx)
		for _, target := range targets {
			fmt.Fprintf(&b, "    - %s\n", yamlScalar(target))
		}
	}

	fmt.Fprintf(&b, `
# set this to true if you want the controls inverted (i.e. top is 0%%, bottom is 100%%)
invert_sliders: %t

# settings for connecting to the arduino/esp board
# use 'auto' to pick the first usb serial device, or a port name like COM4 (windows), /dev/ttyUSB0 or /dev/serial/by-id/... (linux)
com_port: %s
baud_rate: %d

# the highest value your board sends for a slider: 1023 for arduino boards (10-bit), 4095 for ESP32 boards (12-bit)
slider_max_value: %d

# adjust the amount of signal noise reduction depending on your hardware quality
# supported values are "low" (excellent hardware), "default" (regular hardware) or "high" (bad, noisy hardware)
noise_reduction: %s
`, c.InvertSliders, yamlScalar(c.COMPort), c.BaudRate, c.SliderMaxValue, yamlScalar(c.NoiseReduction))

	// carry over any settings we don't know about from the previous file, so they don't get lost
	if extra := unknownConfigKeys(previous); len(extra) > 0 {
		extraYAML, err := yaml.Marshal(extra)
		if err != nil {
			return nil, fmt.Errorf("marshal extra config keys: %w", err)
		}

		b.WriteString("\n# other settings\n")
		b.Write(extraYAML)
	}

	return b.Bytes(), nil
}

func unknownConfigKeys(previous []byte) yaml.MapSlice {
	known := map[string]bool{
		configKeySliderMapping:       true,
		configKeyInvertSliders:       true,
		configKeyCOMPort:             true,
		configKeyBaudRate:            true,
		configKeyNoiseReductionLevel: true,
		configKeySliderMaxValue:      true,
	}

	var existing yaml.MapSlice

	// an unparseable previous file (the reason someone would use the editor, perhaps) shouldn't block saving
	if err := yaml.Unmarshal(previous, &existing); err != nil {
		return nil
	}

	extra := yaml.MapSlice{}

	for _, item := range existing {
		if key, ok := item.Key.(string); ok && known[strings.ToLower(key)] {
			continue
		}

		extra = append(extra, item)
	}

	return extra
}

// formats a string as a yaml scalar, quoting it only if needed (i.e. "yes", or names containing ": ")
func yamlScalar(value string) string {
	marshaled, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%q", value)
	}

	return strings.TrimSuffix(string(marshaled), "\n")
}

// saveEditableValues validates and writes the given values to the config file
func (cc *CanonicalConfig) saveEditableValues(c editableConfig) error {
	if err := writeConfigFile(c); err != nil {
		cc.logger.Warnw("Failed to save config from slider editor", "error", err)
		return err
	}

	cc.logger.Infow("Saved config from slider editor", "path", userConfigFilepath)

	return nil
}
