package deej

import (
	"sync"
	"testing"

	"go.uber.org/zap"
)

type recordingNotifier struct {
	lock   sync.Mutex
	titles []string
}

func (n *recordingNotifier) Notify(title string, message string) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.titles = append(n.titles, title)
}

func newTestSerialIO(maxValue int) (*SerialIO, *recordingNotifier, chan SliderMoveEvent) {
	notifier := &recordingNotifier{}
	d := &Deej{
		logger:   zap.NewNop().Sugar(),
		notifier: notifier,
		config:   &CanonicalConfig{SliderMaxValue: maxValue, NoiseReductionLevel: defaultNoiseReductionLevel},
	}

	sio := &SerialIO{deej: d, logger: d.logger}
	events := make(chan SliderMoveEvent, 100)
	sio.sliderMoveConsumers = []chan SliderMoveEvent{events}

	return sio, notifier, events
}

func drain(events chan SliderMoveEvent) map[int]float32 {
	result := map[int]float32{}

	for {
		select {
		case event := <-events:
			result[event.SliderID] = event.PercentValue
		default:
			return result
		}
	}
}

func TestHandleLineESP32Values(t *testing.T) {
	sio, _, events := newTestSerialIO(4095)

	sio.handleLine(sio.logger, "4095|2048|0|1024\r\n")

	got := drain(events)
	want := map[int]float32{0: 1, 1: 0.5, 2: 0, 3: 0.25}

	for slider, value := range want {
		if got[slider] != value {
			t.Errorf("slider %d: expected %.2f, got %.2f", slider, value, got[slider])
		}
	}

	state := sio.state()
	if len(state.RawValues) != 4 || state.RawValues[1] != 2048 {
		t.Errorf("expected raw values to be kept for the editor, got %v", state.RawValues)
	}
}

func TestHandleLineNeverExceedsFullVolume(t *testing.T) {

	// a 12-bit board on the default (arduino) setting must not push volumes past 100%
	sio, notifier, events := newTestSerialIO(1023)

	sio.handleLine(sio.logger, "512|4095\r\n")

	got := drain(events)
	if got[1] != 1 {
		t.Errorf("expected out of range value to be clamped to 1.0, got %.2f", got[1])
	}

	if state := sio.state(); state.RawValues[1] != 4095 {
		t.Errorf("expected the unclamped raw value to be kept, got %d", state.RawValues[1])
	}

	// and the user should hear about it (once) if it keeps happening
	for i := 0; i < outOfRangeLinesBeforeWarning*2; i++ {
		sio.handleLine(sio.logger, "512|4095\r\n")
	}

	if len(notifier.titles) != 1 {
		t.Errorf("expected exactly one warning notification, got %v", notifier.titles)
	}
}

func TestHandleLineIgnoresGarbage(t *testing.T) {
	sio, notifier, events := newTestSerialIO(1023)

	// a partial first line glued to a full one, and other junk
	for _, line := range []string{"4558|925|41\r\n", "hello\r\n", "1|2|\r\n"} {
		sio.handleLine(sio.logger, line)
	}

	if got := drain(events); len(got) != 0 {
		t.Errorf("expected no move events from garbage, got %v", got)
	}

	if len(notifier.titles) != 0 {
		t.Errorf("expected no notifications from garbage, got %v", notifier.titles)
	}
}

func TestResyncSlidersReemitsAllValues(t *testing.T) {
	sio, _, events := newTestSerialIO(1023)

	sio.handleLine(sio.logger, "1023|0\r\n")
	drain(events)

	// unchanged values don't produce events...
	sio.handleLine(sio.logger, "1023|0\r\n")
	if got := drain(events); len(got) != 0 {
		t.Fatalf("expected no events for unchanged values, got %v", got)
	}

	// ...unless a resync was requested
	sio.resyncSliders()
	sio.handleLine(sio.logger, "1023|0\r\n")
	if got := drain(events); len(got) != 2 {
		t.Errorf("expected events for both sliders after resync, got %v", got)
	}
}
