package deej

// SessionFinder represents an entity that can find all current audio sessions
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	Release() error
}

// sessionChangeNotifier is optionally implemented by session finders that can tell when audio sessions
// or devices come and go, so that new sessions pick up their slider's volume without having to wait for a refresh
type sessionChangeNotifier interface {
	SessionChanges() <-chan struct{}
}
