package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// guards the connection state below
	connLock           sync.Mutex
	conn               io.ReadWriteCloser
	connected          bool
	connOptions        serial.OpenOptions
	configuredPort     string // the port as it appears in the config (i.e. "auto"), used to detect changes
	configuredBaudRate int
	stopped            bool // set by Stop, keeps us from reconnecting
	reconnecting       bool // whether a background reconnect loop is running

	// guards the slider state below, which is also read by the slider editor
	valuesLock                 sync.Mutex
	lastKnownNumSliders        int
	currentSliderPercentValues []float32
	currentSliderRawValues     []int
	lastLineTime               time.Time
	highestRawValue            int
	outOfRangeLines            int
	warnedAboutRange           bool

	sliderMoveConsumers []chan SliderMoveEvent
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

// serialState describes the board connection, as shown in the slider editor
type serialState struct {
	Connected bool      `json:"connected"`
	Port      string    `json:"port"`
	Receiving bool      `json:"receiving"`
	Values    []float32 `json:"values"`
	RawValues []int     `json:"rawValues"`
}

const (

	// how often to try reconnecting after the board was unplugged (or wasn't there to begin with)
	serialReconnectInterval = 2 * time.Second

	// how recently a line must have arrived for the board to count as sending data
	serialReceivingTimeout = time.Second

	// how many consecutive lines with values above slider_max_value we tolerate before telling the user their setting looks wrong
	outOfRangeLinesBeforeWarning = 50
)

// up to 5 digits per value, so 12-bit (4095) and even 16-bit boards fit
var expectedLinePattern = regexp.MustCompile(`^\d{1,5}(\|\d{1,5})*\r\n$`)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		connected:           false,
		conn:                nil,
		sliderMoveConsumers: []chan SliderMoveEvent{},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to our arduino chip. If that fails, deej keeps trying in the background
// (so plugging the board in later just works), and the error is returned to let the caller notify the user
func (sio *SerialIO) Start() error {
	sio.connLock.Lock()
	defer sio.connLock.Unlock()

	sio.stopped = false

	if err := sio.connect(); err != nil {
		sio.logger.Warnw("Failed to open serial connection", "error", err)
		sio.reconnectInBackground()

		return err
	}

	return nil
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	sio.connLock.Lock()
	defer sio.connLock.Unlock()

	sio.stopped = true

	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.closeConn(sio.logger)
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

// notifyConnectError tells the user why we couldn't connect, and how to fix it
func (sio *SerialIO) notifyConnectError(err error) {
	port, _ := sio.deej.config.connectionInfo()
	title := fmt.Sprintf("Can't connect to %s!", port)

	switch {
	case errors.Is(err, os.ErrPermission):
		sio.deej.notifier.Notify(title, util.SerialPortPermissionHint())

	case errors.Is(err, os.ErrNotExist) && util.IsAutoSerialPort(port):
		sio.deej.notifier.Notify("Can't find your board!",
			"No USB serial device found. deej will connect as soon as you plug it in.")

	case errors.Is(err, os.ErrNotExist):
		sio.deej.notifier.Notify(title,
			"This serial port doesn't exist. deej will connect when it shows up - check your board and configuration.")

	default:
		sio.deej.notifier.Notify(title, "Please check deej's logs for more details.")
	}
}

// resyncSliders makes the next line from the board emit move events for all sliders,
// which re-applies every slider's volume to its targets
func (sio *SerialIO) resyncSliders() {
	sio.valuesLock.Lock()
	defer sio.valuesLock.Unlock()

	sio.lastKnownNumSliders = 0
}

// state returns the current connection state and slider values, for the slider editor
func (sio *SerialIO) state() serialState {
	sio.connLock.Lock()
	result := serialState{
		Connected: sio.connected,
		Port:      sio.connOptions.PortName,
	}
	sio.connLock.Unlock()

	sio.valuesLock.Lock()
	defer sio.valuesLock.Unlock()

	result.Receiving = result.Connected && time.Since(sio.lastLineTime) < serialReceivingTimeout
	result.Values = append([]float32{}, sio.currentSliderPercentValues...)
	result.RawValues = append([]int{}, sio.currentSliderRawValues...)

	return result
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:

				// make any config reload unset our slider number to ensure process volumes are being re-set
				// (the next read line will emit SliderMoveEvent instances for all sliders)\
				// this needs to happen after a small delay, because the session map will also re-acquire sessions
				// whenever the config file is reloaded, and we don't want it to receive these move events while the map
				// is still cleared. this is kind of ugly, but shouldn't cause any issues
				go func() {
					<-time.After(stopDelay)
					sio.resyncSliders()
				}()

				// if connection params have changed, attempt to stop and start the connection
				comPort, baudRate := sio.deej.config.connectionInfo()

				sio.connLock.Lock()
				connectionChanged := comPort != sio.configuredPort || baudRate != sio.configuredBaudRate
				sio.connLock.Unlock()

				if connectionChanged {
					sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
					sio.Stop()

					// let the connection close
					<-time.After(stopDelay)

					if err := sio.Start(); err != nil {
						sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
						sio.notifyConnectError(err)
					} else {
						sio.logger.Debug("Renewed connection successfully")
					}
				}
			}
		}
	}()
}

// connect opens the configured serial port and starts reading from it. the caller must hold connLock
func (sio *SerialIO) connect() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	sio.configuredPort, sio.configuredBaudRate = sio.deej.config.connectionInfo()

	portName := sio.configuredPort

	// pick the most likely port on our own, if asked to
	if util.IsAutoSerialPort(portName) {
		ports, err := util.FindSerialPorts()
		if err != nil || len(ports) == 0 {
			return fmt.Errorf("auto-detect serial port (%v): %w", err, os.ErrNotExist)
		}

		portName = ports[0].Path
		sio.logger.Debugw("Auto-detected serial port", "port", portName, "description", ports[0].Description)
	}

	// set minimum read size according to platform (0 for windows, 1 for linux)
	// this prevents a rare bug on windows where serial reads get congested,
	// resulting in significant lag
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	sio.connOptions = serial.OpenOptions{
		PortName:        portName,
		BaudRate:        uint(sio.configuredBaudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: uint(minimumReadSize),
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", sio.connOptions.PortName,
		"baudRate", sio.connOptions.BaudRate,
		"minReadSize", minimumReadSize)

	conn, err := serial.Open(sio.connOptions)
	if err != nil {
		return fmt.Errorf("open serial connection: %w", err)
	}

	namedLogger := sio.logger.Named(strings.ToLower(filepath.Base(sio.connOptions.PortName)))

	namedLogger.Infow("Connected", "conn", conn)
	sio.conn = conn
	sio.connected = true

	// a fresh connection (or a different board) means volumes should be re-applied from the current slider positions
	sio.resyncSliders()

	go sio.readLoop(conn, namedLogger)

	return nil
}

// closeConn closes the active connection. the caller must hold connLock
func (sio *SerialIO) closeConn(logger *zap.SugaredLogger) {
	if err := sio.conn.Close(); err != nil {
		logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		logger.Debug("Serial connection closed")
	}

	sio.conn = nil
	sio.connected = false
}

// reconnectInBackground keeps trying to connect until it succeeds or Stop is called. the caller must hold connLock
func (sio *SerialIO) reconnectInBackground() {
	if sio.reconnecting || sio.stopped {
		return
	}

	sio.reconnecting = true

	go func() {
		for {
			<-time.After(serialReconnectInterval)

			sio.connLock.Lock()

			if sio.stopped || sio.connected {
				sio.reconnecting = false
				sio.connLock.Unlock()

				return
			}

			if err := sio.connect(); err != nil {
				sio.logger.Debugw("Failed to reconnect, will try again", "error", err)
				sio.connLock.Unlock()

				continue
			}

			sio.reconnecting = false
			port := sio.connOptions.PortName
			sio.connLock.Unlock()

			sio.logger.Infow("Reconnected", "port", port)
			sio.deej.notifier.Notify(fmt.Sprintf("Connected to %s", port), "Your sliders are good to go.")

			return
		}
	}()
}

func (sio *SerialIO) readLoop(conn io.ReadWriteCloser, logger *zap.SugaredLogger) {
	reader := bufio.NewReader(conn)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if sio.deej.Verbose() {
				logger.Warnw("Failed to read line from serial", "error", err, "line", line)
			}

			break
		}

		if sio.deej.Verbose() {
			logger.Debugw("Read new line", "line", line)
		}

		sio.handleLine(logger, line)
	}

	sio.connLock.Lock()
	defer sio.connLock.Unlock()

	// if this isn't the active connection anymore, it was closed on purpose (deej is stopping, or the port changed)
	if sio.conn != conn {
		return
	}

	// otherwise, the board was most likely unplugged or reset
	logger.Warn("Lost serial connection, will keep trying to reconnect")
	port := sio.connOptions.PortName
	sio.closeConn(logger)

	sio.deej.notifier.Notify(fmt.Sprintf("Lost connection to %s", port),
		"deej will reconnect automatically once your board is back.")

	sio.reconnectInBackground()
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	if !expectedLinePattern.MatchString(line) {
		return
	}

	// trim the suffix
	line = strings.TrimSuffix(line, "\r\n")

	// split on pipe (|), this gives a slice of numerical strings between "0" and the board's max value (i.e. "1023")
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)
	maxValue, invertSliders, noiseReductionLevel := sio.deej.config.sliderSettings()

	sio.valuesLock.Lock()
	sio.lastLineTime = time.Now()

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)
		sio.currentSliderRawValues = make([]int, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	lineOutOfRange := false

	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)

		if number > maxValue {
			lineOutOfRange = true

			// turns out the first line could come out dirty sometimes (i.e. "4558|925|41|643|220")
			// so let's check the first number for correctness just in case
			if sliderIdx == 0 {
				sio.logger.Debugw("Got malformed line from serial, ignoring", "line", line)
				break
			}
		}

		// keep the unclamped value around, since it helps with figuring out the right slider_max_value
		sio.currentSliderRawValues[sliderIdx] = number

		if number > sio.highestRawValue {
			sio.highestRawValue = number
		}

		// never go above 100%, even if slider_max_value is set lower than what the board sends
		if number > maxValue {
			number = maxValue
		}

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / float32(maxValue)

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if invertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, noiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	// a board that keeps sending values above slider_max_value is most likely a 12-bit board (like the ESP32)
	// running with the default arduino setting, so let the user know (once)
	warnAboutRange := false
	highestRawValue := sio.highestRawValue

	if !lineOutOfRange {
		sio.outOfRangeLines = 0
	} else if sio.outOfRangeLines++; sio.outOfRangeLines >= outOfRangeLinesBeforeWarning && !sio.warnedAboutRange {
		sio.warnedAboutRange = true
		warnAboutRange = true
	}

	sio.valuesLock.Unlock()

	if warnAboutRange {
		logger.Warnw("Board is sending values above slider_max_value",
			"sliderMaxValue", maxValue,
			"highestRawValue", highestRawValue)

		sio.deej.notifier.Notify("Your sliders go higher than expected",
			fmt.Sprintf("Your board sends values up to %d, but slider_max_value is %d. For ESP32 boards, set it to 4095.",
				highestRawValue, maxValue))
	}

	// deliver move events if there are any, towards all potential consumers.
	// this happens outside of valuesLock, since consumers may call back into us (i.e. resyncSliders)
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}
