package deej

import (
	"strings"

	"go.uber.org/zap"
)

// Session represents a single addressable audio session
type Session interface {
	GetVolume() float32
	SetVolume(v float32) error

	// TODO: future mute support
	// GetMute() bool
	// SetMute(m bool) error

	Key() string
	Release()
}

const (

	// ideally these would share a common ground in baseSession
	// but it will not call the child GetVolume correctly :/
	sessionCreationLogMessage = "Created audio session instance"

	// format this with s.humanReadableDesc and whatever the current volume is
	sessionStringFormat = "<session: %s, vol: %.2f>"
)

type baseSession struct {
	logger *zap.SugaredLogger
	system bool
	master bool

	// used by Key(), needs to be set by child
	name string

	// used by String(), needs to be set by child
	humanReadableDesc string
}

func (s *baseSession) Key() string {
	if s.system {
		return systemSessionName
	}

	if s.master {
		return strings.ToLower(s.name) // could be master or mic, or any device's friendly name
	}

	return strings.ToLower(s.name)
}

// isDevice returns true for sessions that control an entire audio device (master, mic or a device bound by name)
// rather than a single app. these never count as "unmapped"
func (s *baseSession) isDevice() bool {
	return s.master
}

// friendlyName returns the session's name before it's lowercased (i.e. "Discord" instead of "discord")
func (s *baseSession) friendlyName() string {
	return s.name
}

// sessionInfo is implemented by all sessions through baseSession
type sessionInfo interface {
	isDevice() bool
	friendlyName() string
}

func isDeviceSession(session Session) bool {
	info, ok := session.(sessionInfo)
	return ok && info.isDevice()
}
