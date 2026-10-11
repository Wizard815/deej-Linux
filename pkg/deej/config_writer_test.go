package deej

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestRenderConfigFileRoundTrip(t *testing.T) {
	config := editableConfig{
		SliderMapping: map[int][]string{
			0: {"master"},
			1: {"spotify", "Discord"},
			3: {"Speakers (Realtek High Definition Audio)"},
			4: {"yes", "weird: name", "#hashtag"},
		},
		InvertSliders:  true,
		COMPort:        "/dev/serial/by-id/usb-Silicon_Labs_CP2102-if00-port0",
		BaudRate:       115200,
		NoiseReduction: "high",
		SliderMaxValue: 4095,
	}

	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	rendered, err := renderConfigFile(config, nil)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		SliderMapping  map[int]interface{} `yaml:"slider_mapping"`
		InvertSliders  bool                `yaml:"invert_sliders"`
		COMPort        string              `yaml:"com_port"`
		BaudRate       int                 `yaml:"baud_rate"`
		SliderMaxValue int                 `yaml:"slider_max_value"`
		NoiseReduction string              `yaml:"noise_reduction"`
	}

	if err := yaml.Unmarshal(rendered, &parsed); err != nil {
		t.Fatalf("rendered config isn't valid yaml: %v\n%s", err, rendered)
	}

	for sliderIdx, targets := range config.SliderMapping {
		var got []string

		switch value := parsed.SliderMapping[sliderIdx].(type) {
		case string:
			got = []string{value}
		case []interface{}:
			for _, item := range value {
				got = append(got, item.(string))
			}
		}

		if !reflect.DeepEqual(got, targets) {
			t.Errorf("slider %d: expected %v, got %v", sliderIdx, targets, got)
		}
	}

	if !parsed.InvertSliders || parsed.COMPort != config.COMPort || parsed.BaudRate != 115200 ||
		parsed.SliderMaxValue != 4095 || parsed.NoiseReduction != "high" {
		t.Errorf("settings didn't survive the round trip: %+v", parsed)
	}
}

func TestRenderConfigFileKeepsUnknownSettings(t *testing.T) {
	previous := []byte("slider_mapping:\n  0: master\ncom_port: COM4\nsome_future_setting: 42\n")

	rendered, err := renderConfigFile(defaultEditableConfig(), previous)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(rendered), "some_future_setting: 42") {
		t.Errorf("expected unknown setting to be carried over:\n%s", rendered)
	}

	if strings.Count(string(rendered), "com_port:") != 1 {
		t.Errorf("expected known settings not to be duplicated:\n%s", rendered)
	}
}

func TestNormalizeEditableConfig(t *testing.T) {
	config := editableConfig{
		SliderMapping:  map[int][]string{0: {" master ", "MASTER", ""}, 2: {}},
		COMPort:        "",
		BaudRate:       9600,
		SliderMaxValue: 1023,
	}

	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(config.SliderMapping, map[int][]string{0: {"master"}}) {
		t.Errorf("expected targets to be trimmed and deduplicated, got %v", config.SliderMapping)
	}

	if config.COMPort != "auto" || config.NoiseReduction != "default" {
		t.Errorf("expected defaults to be filled in, got port %q and noise reduction %q", config.COMPort, config.NoiseReduction)
	}

	for name, invalid := range map[string]editableConfig{
		"zero baud rate":  {BaudRate: 0, SliderMaxValue: 1023},
		"zero max value":  {BaudRate: 9600, SliderMaxValue: 0},
		"bad noise level": {BaudRate: 9600, SliderMaxValue: 1023, NoiseReduction: "extreme"},
		"bad slider":      {BaudRate: 9600, SliderMaxValue: 1023, SliderMapping: map[int][]string{-1: {"master"}}},
	} {
		if err := invalid.normalize(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
