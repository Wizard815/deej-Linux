// deej for ESP32 boards
//
// pair this with these settings in deej's config.yaml (or pick "ESP32 · 12-bit" in the slider editor):
//   baud_rate: 115200
//   slider_max_value: 4095
//
// wiring: connect each slider's outer pins to 3.3V and GND, and its middle (wiper) pin to one of the pins below.
// important: never power the sliders from 5V/VIN - ESP32 pins aren't 5V tolerant!
//
// only use ADC1 pins (GPIO 32-39 on the classic ESP32): ADC2 pins stop working whenever wifi is on.
// for other chips, pick ADC1 pins from your board's pinout instead, for example:
//   ESP32-S2/S3: GPIO 1-10
//   ESP32-C3:    GPIO 0-4
// ESP8266 boards only have a single analog pin (A0), so they need an analog multiplexer (like the CD4051) for
// more than one slider.

const int NUM_SLIDERS = 5;
const int analogInputs[NUM_SLIDERS] = {36, 39, 34, 35, 32};

// the ESP32's ADC is noisy, so every reading is an average of several samples, smoothed over time.
// if volumes still jitter, set noise_reduction to "high" in deej's config
const int SAMPLES_PER_READING = 16;
const float SMOOTHING = 0.3; // 0..1, lower is smoother (but slower to follow the slider)

float smoothedValues[NUM_SLIDERS];

void setup() {
  // values from 0 to 4095, across the full 0-3.3V range (that's the default attenuation on all ESP32 cores)
  analogReadResolution(12);

  for (int i = 0; i < NUM_SLIDERS; i++) {
    pinMode(analogInputs[i], INPUT);
    smoothedValues[i] = readSlider(i);
  }

  Serial.begin(115200);
}

void loop() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
    smoothedValues[i] += SMOOTHING * (readSlider(i) - smoothedValues[i]);
  }

  sendSliderValues();
  delay(10);
}

float readSlider(int index) {
  long total = 0;

  for (int sample = 0; sample < SAMPLES_PER_READING; sample++) {
    total += analogRead(analogInputs[index]);
  }

  return (float)total / SAMPLES_PER_READING;
}

// sends a line like "4095|2048|0|1024|3000" - the same format as the arduino sketch, just with bigger numbers
void sendSliderValues() {
  String builtString = String("");

  for (int i = 0; i < NUM_SLIDERS; i++) {
    builtString += String((int)(smoothedValues[i] + 0.5));

    if (i < NUM_SLIDERS - 1) {
      builtString += String("|");
    }
  }

  Serial.println(builtString);
}
