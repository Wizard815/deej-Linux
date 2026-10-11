package util

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// there's no single way to ask linux which window is focused - it depends on the display server and the desktop.
// each provider below knows one way, and they're tried in order until one of them answers

type focusProvider interface {

	// whether this provider applies to the current session at all (based on environment variables)
	available() bool

	// returns the PID owning the focused window, or 0 if no window is focused
	activePID() (int, error)
}

const (
	focusIPCTimeout = 250 * time.Millisecond
)

var focusProviders = []focusProvider{
	&hyprlandFocusProvider{},
	&swayFocusProvider{},
	&niriFocusProvider{},
	&kwinFocusProvider{},
	&gnomeFocusProvider{},
	&x11FocusProvider{},
}

func getCurrentWindowProcessNames() ([]string, error) {

	// apply an internal cooldown on this function to avoid asking the compositor too frequently.
	// return a cached value during that cooldown
	now := time.Now()
	if lastGetCurrentWindowCall.Add(getCurrentWindowInternalCooldown).After(now) {
		return lastGetCurrentWindowResult, lastGetCurrentWindowError
	}

	lastGetCurrentWindowCall = now

	pid, err := activeWindowPID()
	if err != nil || pid == 0 {
		lastGetCurrentWindowResult, lastGetCurrentWindowError = nil, err
		return nil, err
	}

	// audio is often played by a child of the process owning the window (browsers, wine, launchers),
	// so include the names of all its descendants as well. looking these up is cheap
	result := []string{}
	for _, processID := range append([]int{pid}, descendantPIDs(pid)...) {
		result = append(result, processNames(processID)...)
	}

	lastGetCurrentWindowResult, lastGetCurrentWindowError = result, nil
	return result, nil
}

func activeWindowPID() (int, error) {
	var firstErr error

	for _, provider := range focusProviders {
		if !provider.available() {
			continue
		}

		pid, err := provider.activePID()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}

			continue
		}

		// the X11 fallback only sees XWayland windows, so if an earlier provider failed to set up, surface its error
		// instead of claiming that nothing is focused
		if pid == 0 && firstErr != nil {
			return 0, firstErr
		}

		return pid, nil
	}

	if firstErr == nil {
		firstErr = errors.New("no supported display server found (DISPLAY and WAYLAND_DISPLAY are unset)")
	}

	return 0, firstErr
}

// returns the names a process might be known by in the audio server: its executable's name, and its argv[0] and
// comm (which differ for e.g. wrapper scripts, or apps that rename their processes)
func processNames(pid int) []string {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	result := []string{}

	if exe, err := os.Readlink(filepath.Join(procDir, "exe")); err == nil {
		result = append(result, filepath.Base(strings.TrimSuffix(exe, " (deleted)")))
	}

	if cmdline, err := ioutil.ReadFile(filepath.Join(procDir, "cmdline")); err == nil && len(cmdline) > 0 {
		argv0 := strings.SplitN(string(cmdline), "\x00", 2)[0]
		if argv0 != "" {
			result = append(result, filepath.Base(argv0))
		}
	}

	if comm, err := ioutil.ReadFile(filepath.Join(procDir, "comm")); err == nil {
		result = append(result, strings.TrimSpace(string(comm)))
	}

	return result
}

// returns the PIDs of all processes that descend from the given one
func descendantPIDs(pid int) []int {
	entries, err := ioutil.ReadDir("/proc")
	if err != nil {
		return nil
	}

	children := map[int][]int{}

	for _, entry := range entries {
		childPID, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		stat, err := ioutil.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}

		// the format is "pid (comm) state ppid ...", where comm may itself contain spaces and parentheses
		closingParen := strings.LastIndexByte(string(stat), ')')
		if closingParen < 0 {
			continue
		}

		fields := strings.Fields(string(stat[closingParen+1:]))
		if len(fields) < 2 {
			continue
		}

		parentPID, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}

		children[parentPID] = append(children[parentPID], childPID)
	}

	result := []int{}
	queue := children[pid]

	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]

		result = append(result, next)
		queue = append(queue, children[next]...)
	}

	return result
}

func waylandSession() bool {
	return os.Getenv("WAYLAND_DISPLAY") != ""
}

func desktopIs(name string) bool {
	for _, desktop := range strings.Split(os.Getenv("XDG_CURRENT_DESKTOP"), ":") {
		if strings.EqualFold(desktop, name) {
			return true
		}
	}

	return false
}

// hyprland: ask its IPC socket for the active window

type hyprlandFocusProvider struct{}

func (p *hyprlandFocusProvider) available() bool {
	return os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != ""
}

func (p *hyprlandFocusProvider) activePID() (int, error) {
	signature := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE")

	// newer versions keep the socket in the runtime dir, older ones in /tmp
	socketPaths := []string{
		filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "hypr", signature, ".socket.sock"),
		filepath.Join("/tmp", "hypr", signature, ".socket.sock"),
	}

	var conn net.Conn
	var err error

	for _, socketPath := range socketPaths {
		if conn, err = net.DialTimeout("unix", socketPath, focusIPCTimeout); err == nil {
			break
		}
	}

	if err != nil {
		return 0, fmt.Errorf("connect to hyprland socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(focusIPCTimeout))

	if _, err := conn.Write([]byte("j/activewindow")); err != nil {
		return 0, fmt.Errorf("query hyprland active window: %w", err)
	}

	response, err := ioutil.ReadAll(conn)
	if err != nil {
		return 0, fmt.Errorf("read hyprland active window: %w", err)
	}

	var window struct {
		PID int `json:"pid"`
	}

	if err := json.Unmarshal(response, &window); err != nil {
		return 0, fmt.Errorf("parse hyprland active window: %w", err)
	}

	return positivePID(window.PID), nil
}

// sway (and i3): find the focused node in the tree returned by the i3 IPC protocol

type swayFocusProvider struct{}

const (
	i3IPCMagic       = "i3-ipc"
	i3IPCMessageTree = 4
)

func (p *swayFocusProvider) socketPath() string {
	if socketPath := os.Getenv("SWAYSOCK"); socketPath != "" {
		return socketPath
	}

	return os.Getenv("I3SOCK")
}

func (p *swayFocusProvider) available() bool {
	return p.socketPath() != ""
}

func (p *swayFocusProvider) activePID() (int, error) {
	conn, err := net.DialTimeout("unix", p.socketPath(), focusIPCTimeout)
	if err != nil {
		return 0, fmt.Errorf("connect to sway socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(focusIPCTimeout))

	// a message is the magic string, followed by the payload length and message type (in native byte order)
	header := make([]byte, len(i3IPCMagic)+8)
	copy(header, i3IPCMagic)
	binary.NativeEndian.PutUint32(header[len(i3IPCMagic):], 0)
	binary.NativeEndian.PutUint32(header[len(i3IPCMagic)+4:], i3IPCMessageTree)

	if _, err := conn.Write(header); err != nil {
		return 0, fmt.Errorf("query sway tree: %w", err)
	}

	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, fmt.Errorf("read sway reply header: %w", err)
	}

	payload := make([]byte, binary.NativeEndian.Uint32(header[len(i3IPCMagic):]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, fmt.Errorf("read sway tree: %w", err)
	}

	var tree swayNode
	if err := json.Unmarshal(payload, &tree); err != nil {
		return 0, fmt.Errorf("parse sway tree: %w", err)
	}

	return positivePID(tree.focusedPID()), nil
}

type swayNode struct {
	Focused       bool       `json:"focused"`
	PID           int        `json:"pid"`
	Nodes         []swayNode `json:"nodes"`
	FloatingNodes []swayNode `json:"floating_nodes"`
}

func (n *swayNode) focusedPID() int {
	if n.Focused {
		return n.PID
	}

	for _, children := range [][]swayNode{n.Nodes, n.FloatingNodes} {
		for i := range children {
			if pid := children[i].focusedPID(); pid != 0 {
				return pid
			}
		}
	}

	return 0
}

// niri: ask its IPC socket for the focused window

type niriFocusProvider struct{}

func (p *niriFocusProvider) available() bool {
	return os.Getenv("NIRI_SOCKET") != ""
}

func (p *niriFocusProvider) activePID() (int, error) {
	conn, err := net.DialTimeout("unix", os.Getenv("NIRI_SOCKET"), focusIPCTimeout)
	if err != nil {
		return 0, fmt.Errorf("connect to niri socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(focusIPCTimeout))

	if _, err := conn.Write([]byte("\"FocusedWindow\"\n")); err != nil {
		return 0, fmt.Errorf("query niri focused window: %w", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return 0, fmt.Errorf("read niri focused window: %w", err)
	}

	var reply struct {
		Ok struct {
			FocusedWindow *struct {
				PID int `json:"pid"`
			}
		}
		Err string
	}

	if err := json.Unmarshal(line, &reply); err != nil {
		return 0, fmt.Errorf("parse niri focused window: %w", err)
	}

	if reply.Err != "" {
		return 0, fmt.Errorf("niri focused window: %s", reply.Err)
	}

	if reply.Ok.FocusedWindow == nil {
		return 0, nil
	}

	return positivePID(reply.Ok.FocusedWindow.PID), nil
}

// kde plasma (wayland): kwin doesn't expose the active window directly, but it runs scripts that can call out over
// dbus. we load a tiny script that reports the PID of every newly activated window back to us

type kwinFocusProvider struct {
	setupOnce sync.Once
	setupErr  error
	pid       int32
}

const (
	kwinScriptPluginName = "deej-focus-tracker"
	kwinReceiverPath     = "/io/github/deej/FocusTracker"
	kwinReceiverIface    = "io.github.deej.FocusTracker"

	// plasma 6 renamed clients to windows, so support both
	kwinFocusScript = `function deejReport(window) {
	callDBus("%s", "%s", "%s", "Update", window ? String(window.pid) : "0");
}

if (typeof workspace.windowActivated !== "undefined") {
	workspace.windowActivated.connect(deejReport);
	deejReport(workspace.activeWindow);
} else {
	workspace.clientActivated.connect(deejReport);
	deejReport(workspace.activeClient);
}
`
)

// kwinFocusReceiver is exported over dbus for the kwin script to call
type kwinFocusReceiver struct {
	provider *kwinFocusProvider
}

// Update is called by the kwin script whenever a window is activated
func (r *kwinFocusReceiver) Update(pid string) *dbus.Error {
	parsed, _ := strconv.Atoi(pid)
	atomic.StoreInt32(&r.provider.pid, int32(parsed))

	return nil
}

func (p *kwinFocusProvider) available() bool {
	return waylandSession() && desktopIs("KDE")
}

func (p *kwinFocusProvider) activePID() (int, error) {
	p.setupOnce.Do(func() {
		p.setupErr = p.setup()
	})

	if p.setupErr != nil {
		return 0, p.setupErr
	}

	return positivePID(int(atomic.LoadInt32(&p.pid))), nil
}

func (p *kwinFocusProvider) setup() error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return fmt.Errorf("connect to session bus: %w", err)
	}

	if err := conn.Export(&kwinFocusReceiver{provider: p}, kwinReceiverPath, kwinReceiverIface); err != nil {
		return fmt.Errorf("export kwin focus receiver: %w", err)
	}

	// the script calls us back by our unique bus name, so there's no need to own a well-known one
	scriptPath := filepath.Join(os.TempDir(), kwinScriptPluginName+".js")
	script := fmt.Sprintf(kwinFocusScript, conn.Names()[0], kwinReceiverPath, kwinReceiverIface)

	if err := ioutil.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		return fmt.Errorf("write kwin script: %w", err)
	}

	scripting := conn.Object("org.kde.KWin", "/Scripting")

	// unload a leftover script from a previous run, if there is one (this fails harmlessly otherwise)
	scripting.Call("org.kde.kwin.Scripting.unloadScript", 0, kwinScriptPluginName)

	var scriptID int32
	if err := scripting.Call("org.kde.kwin.Scripting.loadScript", 0, scriptPath, kwinScriptPluginName).Store(&scriptID); err != nil {
		return &FocusSetupError{Message: fmt.Sprintf("Couldn't load a KWin script to track the focused app: %v", err)}
	}

	if scriptID < 0 {
		return &FocusSetupError{Message: "KWin refused to load the script that tracks the focused app"}
	}

	// plasma 6 exposes loaded scripts at /Scripting/Script<id>, plasma 5 at /<id>
	for _, scriptPath := range []string{fmt.Sprintf("/Scripting/Script%d", scriptID), fmt.Sprintf("/%d", scriptID)} {
		if err := conn.Object("org.kde.KWin", dbus.ObjectPath(scriptPath)).Call("org.kde.kwin.Script.run", 0).Err; err == nil {
			return nil
		}
	}

	// last resort for older versions: start all loaded scripts
	if err := scripting.Call("org.kde.kwin.Scripting.start", 0).Err; err != nil {
		return &FocusSetupError{Message: fmt.Sprintf("Couldn't start the KWin script that tracks the focused app: %v", err)}
	}

	return nil
}

// gnome (wayland): gnome shell doesn't let apps see other apps' windows, so this relies on the "Window Calls"
// extension (https://extensions.gnome.org/extension/4724/window-calls/) which exposes them over dbus

type gnomeFocusProvider struct{}

func (p *gnomeFocusProvider) available() bool {
	return waylandSession() && desktopIs("GNOME")
}

func (p *gnomeFocusProvider) activePID() (int, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return 0, fmt.Errorf("connect to session bus: %w", err)
	}

	var windowsJSON string

	call := conn.Object("org.gnome.Shell", "/org/gnome/Shell/Extensions/Windows").
		Call("org.gnome.Shell.Extensions.Windows.List", 0)

	if err := call.Store(&windowsJSON); err != nil {
		return 0, &FocusSetupError{
			Message: "To control the focused app on GNOME (Wayland), install and enable the \"Window Calls\" extension",
		}
	}

	var windows []struct {
		PID   int  `json:"pid"`
		Focus bool `json:"focus"`
	}

	if err := json.Unmarshal([]byte(windowsJSON), &windows); err != nil {
		return 0, fmt.Errorf("parse window list from gnome extension: %w", err)
	}

	for _, window := range windows {
		if window.Focus {
			return positivePID(window.PID), nil
		}
	}

	return 0, nil
}

// x11 (and xwayland): read the _NET_ACTIVE_WINDOW and _NET_WM_PID properties set by the window manager

type x11FocusProvider struct {
	conn         *xgb.Conn
	root         xproto.Window
	activeWindow xproto.Atom
	windowPID    xproto.Atom
}

func (p *x11FocusProvider) available() bool {
	return os.Getenv("DISPLAY") != ""
}

func (p *x11FocusProvider) activePID() (int, error) {
	if err := p.connect(); err != nil {
		return 0, err
	}

	pid, err := p.queryActivePID()
	if err != nil {

		// the connection may have gone stale, so make sure to reconnect next time
		p.conn.Close()
		p.conn = nil

		return 0, err
	}

	return pid, nil
}

func (p *x11FocusProvider) connect() error {
	if p.conn != nil {
		return nil
	}

	// xgb logs to stderr by default, and we'd rather report errors ourselves
	xgb.Logger = log.New(ioutil.Discard, "", 0)

	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("connect to X server: %w", err)
	}

	activeWindow, err := internAtom(conn, "_NET_ACTIVE_WINDOW")
	if err != nil {
		conn.Close()
		return err
	}

	windowPID, err := internAtom(conn, "_NET_WM_PID")
	if err != nil {
		conn.Close()
		return err
	}

	p.conn = conn
	p.root = xproto.Setup(conn).DefaultScreen(conn).Root
	p.activeWindow = activeWindow
	p.windowPID = windowPID

	return nil
}

func (p *x11FocusProvider) queryActivePID() (int, error) {
	reply, err := xproto.GetProperty(p.conn, false, p.root, p.activeWindow, xproto.AtomWindow, 0, 1).Reply()
	if err != nil {
		return 0, fmt.Errorf("get active window: %w", err)
	}

	if reply.ValueLen == 0 {
		return 0, nil
	}

	window := xproto.Window(xgb.Get32(reply.Value))
	if window == 0 {
		return 0, nil
	}

	reply, err = xproto.GetProperty(p.conn, false, window, p.windowPID, xproto.AtomCardinal, 0, 1).Reply()
	if err != nil {
		return 0, fmt.Errorf("get active window pid: %w", err)
	}

	// not every window advertises its PID
	if reply.ValueLen == 0 {
		return 0, nil
	}

	return positivePID(int(xgb.Get32(reply.Value))), nil
}

func internAtom(conn *xgb.Conn, name string) (xproto.Atom, error) {
	reply, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, fmt.Errorf("intern atom %s: %w", name, err)
	}

	if reply.Atom == xproto.AtomNone {
		return 0, fmt.Errorf("window manager doesn't support %s", name)
	}

	return reply.Atom, nil
}

func positivePID(pid int) int {
	if pid < 0 {
		return 0
	}

	return pid
}
