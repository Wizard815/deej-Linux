package deej

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	// receives a value whenever streams or devices are added/removed, or the default device changes
	changes chan struct{}
}

// pulseaudio subscription masks and event bits (see pulse/def.h), these aren't exposed by the proto package
const (
	paSubscriptionMaskSink      = 0x0001
	paSubscriptionMaskSource    = 0x0002
	paSubscriptionMaskSinkInput = 0x0004
	paSubscriptionMaskServer    = 0x0080

	paSubscriptionEventFacilityMask = 0x000F
	paSubscriptionEventSink         = 0x0000
	paSubscriptionEventSource       = 0x0001
	paSubscriptionEventSinkInput    = 0x0002
	paSubscriptionEventServer       = 0x0007

	paSubscriptionEventTypeMask = 0x0030
	paSubscriptionEventNew      = 0x0000
	paSubscriptionEventChange   = 0x0010

	// sink inputs with this media role are notification/alert sounds, which we expose as the "system" session
	paEventMediaRole = "event"

	// prefix for device sessions in logger
	deviceSessionFormat = "device.%s"
)

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	client, conn, err := proto.Connect(pulseServerAddress())
	if err != nil {
		logger.Warnw("Failed to establish PulseAudio connection", "error", err)
		return nil, fmt.Errorf("establish PulseAudio connection: %w", err)
	}

	request := proto.SetClientName{
		Props: proto.PropList{
			"application.name": proto.PropListString("deej"),
		},
	}
	reply := proto.SetClientNameReply{}

	if err := client.Request(&request, &reply); err != nil {
		return nil, err
	}

	sf := &paSessionFinder{
		logger:        logger.Named("session_finder"),
		sessionLogger: logger.Named("sessions"),
		client:        client,
		conn:          conn,
		changes:       make(chan struct{}, 1),
	}

	// get notified about new streams and device changes. this isn't critical, since the session map
	// also refreshes itself periodically, so don't fail if it doesn't work out
	client.Callback = sf.handlePulseEvent

	subscribe := proto.Subscribe{
		Mask: paSubscriptionMaskSink | paSubscriptionMaskSource | paSubscriptionMaskSinkInput | paSubscriptionMaskServer,
	}

	if err := client.Request(&subscribe, nil); err != nil {
		sf.logger.Warnw("Failed to subscribe to PulseAudio events", "error", err)
	}

	sf.logger.Debug("Created PA session finder instance")

	return sf, nil
}

func (sf *paSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	// get the master sink session
	masterSink, err := sf.getMasterSinkSession()
	if err == nil {
		sessions = append(sessions, masterSink)
	} else {
		sf.logger.Warnw("Failed to get master audio sink session", "error", err)
	}

	// get the master source session
	masterSource, err := sf.getMasterSourceSession()
	if err == nil {
		sessions = append(sessions, masterSource)
	} else {
		sf.logger.Warnw("Failed to get master audio source session", "error", err)
	}

	// make all devices bindable by their name (as it appears in your sound settings)
	if err := sf.enumerateAndAddDeviceSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate device sessions", "error", err)
	}

	// enumerate sink inputs and add sessions along the way
	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio sessions", "error", err)
		return nil, fmt.Errorf("enumerate audio sessions: %w", err)
	}

	return sessions, nil
}

// SessionChanges returns a channel that receives a value whenever streams or devices are added or removed,
// or the default device changes
func (sf *paSessionFinder) SessionChanges() <-chan struct{} {
	return sf.changes
}

func (sf *paSessionFinder) Release() error {
	if err := sf.conn.Close(); err != nil {
		sf.logger.Warnw("Failed to close PulseAudio connection", "error", err)
		return fmt.Errorf("close PulseAudio connection: %w", err)
	}

	sf.logger.Debug("Released PA session finder instance")

	return nil
}

// this is called from the PulseAudio client's read loop, so it must never block or make requests of its own
func (sf *paSessionFinder) handlePulseEvent(message interface{}) {
	event, ok := message.(*proto.SubscribeEvent)
	if !ok {
		return
	}

	facility := event.Event & paSubscriptionEventFacilityMask
	eventType := event.Event & paSubscriptionEventTypeMask

	var relevant bool

	switch facility {

	// a new stream started playing
	case paSubscriptionEventSinkInput:
		relevant = eventType == paSubscriptionEventNew

	// a device was plugged in or removed (changes are ignored, since setting a device's volume causes one)
	case paSubscriptionEventSink, paSubscriptionEventSource:
		relevant = eventType != paSubscriptionEventChange

	// the default sink or source changed
	case paSubscriptionEventServer:
		relevant = true
	}

	if !relevant {
		return
	}

	// coalesce bursts of events into a single pending notification
	select {
	case sf.changes <- struct{}{}:
	default:
	}
}

func (sf *paSessionFinder) getMasterSinkSession() (Session, error) {
	request := proto.GetSinkInfo{
		SinkIndex: proto.Undefined,
	}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master sink info", "error", err)
		return nil, fmt.Errorf("get master sink info: %w", err)
	}

	// create the master sink session
	sink := newMasterSession(sf.sessionLogger, sf.client, reply.SinkIndex, reply.Channels, true,
		masterSessionName, masterSessionName)

	return sink, nil
}

func (sf *paSessionFinder) getMasterSourceSession() (Session, error) {
	request := proto.GetSourceInfo{
		SourceIndex: proto.Undefined,
	}
	reply := proto.GetSourceInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master source info", "error", err)
		return nil, fmt.Errorf("get master source info: %w", err)
	}

	// create the master source session
	source := newMasterSession(sf.sessionLogger, sf.client, reply.SourceIndex, reply.Channels, false,
		inputSessionName, inputSessionName)

	return source, nil
}

func (sf *paSessionFinder) enumerateAndAddDeviceSessions(sessions *[]Session) error {
	sinks := proto.GetSinkInfoListReply{}

	if err := sf.client.Request(&proto.GetSinkInfoList{}, &sinks); err != nil {
		sf.logger.Warnw("Failed to get sink list", "error", err)
		return fmt.Errorf("get sink list: %w", err)
	}

	for _, sink := range sinks {
		name := deviceDisplayName(sink.Properties, sink.SinkName)

		*sessions = append(*sessions, newMasterSession(sf.sessionLogger, sf.client, sink.SinkIndex, sink.Channels, true,
			name, fmt.Sprintf(deviceSessionFormat, strings.ToLower(name))))
	}

	sources := proto.GetSourceInfoListReply{}

	if err := sf.client.Request(&proto.GetSourceInfoList{}, &sources); err != nil {
		sf.logger.Warnw("Failed to get source list", "error", err)
		return fmt.Errorf("get source list: %w", err)
	}

	for _, source := range sources {

		// skip the "monitor of ..." sources that mirror each output device. in source info replies,
		// this field holds the index of the sink being monitored, if any
		if source.MonitorSourceIndex != proto.Undefined {
			continue
		}

		name := deviceDisplayName(source.Properties, source.SourceName)

		*sessions = append(*sessions, newMasterSession(sf.sessionLogger, sf.client, source.SourceIndex, source.Channels, false,
			name, fmt.Sprintf(deviceSessionFormat, strings.ToLower(name))))
	}

	return nil
}

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	for _, info := range reply {
		name, ok := info.Properties["application.process.binary"]

		// some clients (i.e. sandboxed ones) don't report their binary, fall back to the name they gave themselves
		if !ok {
			name, ok = info.Properties["application.name"]
		}

		if !ok {
			sf.logger.Warnw("Failed to get sink input's process name",
				"sinkInputIndex", info.SinkInputIndex)

			continue
		}

		role, hasRole := info.Properties["media.role"]
		system := hasRole && role.String() == paEventMediaRole

		// create the deej session object
		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name.String(), system)

		// add it to our slice
		*sessions = append(*sessions, newSession)

	}

	return nil
}

// returns the human-readable device name (as it appears in your sound settings), or its internal name as a fallback
func deviceDisplayName(properties proto.PropList, fallback string) string {
	if description, ok := properties["device.description"]; ok && description.String() != "" {
		return description.String()
	}

	return fallback
}

// the pulse library assumes the server lives under /run/user/<uid>, which isn't where every distro keeps the
// runtime directory. returning an empty string lets it use PULSE_SERVER (if set), or that default
func pulseServerAddress() string {
	if _, ok := os.LookupEnv("PULSE_SERVER"); ok {
		return ""
	}

	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" && !strings.ContainsAny(runtimeDir, " \t") {
		return "unix:" + filepath.Join(runtimeDir, "pulse", "native")
	}

	return ""
}
