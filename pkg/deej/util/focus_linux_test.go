package util

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// serves a single connection on a fake compositor socket, answering with whatever respond returns
func serveFakeSocket(t *testing.T, socketPath string, respond func(conn net.Conn)) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		defer conn.Close()
		respond(conn)
	}()
}

func TestHyprlandFocusProvider(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "abc123")

	received := make(chan string, 1)

	serveFakeSocket(t, filepath.Join(runtimeDir, "hypr", "abc123", ".socket.sock"), func(conn net.Conn) {
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
		conn.Write([]byte(`{"address": "0x1234", "class": "firefox", "pid": 4242}`))
	})

	provider := &hyprlandFocusProvider{}
	if !provider.available() {
		t.Fatal("expected provider to be available")
	}

	pid, err := provider.activePID()
	if err != nil {
		t.Fatal(err)
	}

	if pid != 4242 {
		t.Errorf("expected pid 4242, got %d", pid)
	}

	if request := <-received; request != "j/activewindow" {
		t.Errorf("unexpected request %q", request)
	}
}

func TestSwayFocusProvider(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "sway.sock")
	t.Setenv("SWAYSOCK", socketPath)

	// a workspace containing a split container, with the focused window nested inside it
	tree := `{"focused": false, "nodes": [
		{"type": "workspace", "focused": false, "nodes": [
			{"type": "con", "focused": false, "pid": 100, "nodes": []},
			{"type": "con", "focused": false, "nodes": [
				{"type": "con", "focused": true, "pid": 5151, "nodes": []}
			]}
		], "floating_nodes": []}
	]}`

	serveFakeSocket(t, socketPath, func(conn net.Conn) {
		header := make([]byte, 14)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}

		if string(header[:6]) != "i3-ipc" || binary.NativeEndian.Uint32(header[10:]) != i3IPCMessageTree {
			return
		}

		reply := make([]byte, 14)
		copy(reply, "i3-ipc")
		binary.NativeEndian.PutUint32(reply[6:], uint32(len(tree)))
		binary.NativeEndian.PutUint32(reply[10:], i3IPCMessageTree)
		conn.Write(append(reply, tree...))
	})

	pid, err := (&swayFocusProvider{}).activePID()
	if err != nil {
		t.Fatal(err)
	}

	if pid != 5151 {
		t.Errorf("expected pid 5151, got %d", pid)
	}
}

func TestNiriFocusProvider(t *testing.T) {
	for name, testCase := range map[string]struct {
		reply string
		pid   int
	}{
		"focused window":  {`{"Ok":{"FocusedWindow":{"id":3,"title":"Spotify","app_id":"spotify","pid":777}}}`, 777},
		"nothing focused": {`{"Ok":{"FocusedWindow":null}}`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			socketPath := filepath.Join(t.TempDir(), "niri.sock")
			t.Setenv("NIRI_SOCKET", socketPath)

			serveFakeSocket(t, socketPath, func(conn net.Conn) {
				request, _ := bufio.NewReader(conn).ReadString('\n')
				if request == "\"FocusedWindow\"\n" {
					conn.Write([]byte(testCase.reply + "\n"))
				}
			})

			pid, err := (&niriFocusProvider{}).activePID()
			if err != nil {
				t.Fatal(err)
			}

			if pid != testCase.pid {
				t.Errorf("expected pid %d, got %d", testCase.pid, pid)
			}
		})
	}
}

// starts a private dbus session bus for the gnome and kde tests (once, since dbus.SessionBus is process-wide)
var (
	sessionBusOnce    sync.Once
	sessionBusAddress string
	sessionBusErr     error
	sessionBusDaemon  *exec.Cmd
)

func TestMain(m *testing.M) {
	code := m.Run()

	if sessionBusDaemon != nil {
		sessionBusDaemon.Process.Kill()
		sessionBusDaemon.Wait()
	}

	os.Exit(code)
}

func privateSessionBus(t *testing.T) string {
	t.Helper()

	sessionBusOnce.Do(func() {
		if _, err := exec.LookPath("dbus-daemon"); err != nil {
			sessionBusErr = err
			return
		}

		cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			sessionBusErr = err
			return
		}

		if err := cmd.Start(); err != nil {
			sessionBusErr = err
			return
		}

		sessionBusDaemon = cmd

		address, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			sessionBusErr = err
			return
		}

		sessionBusAddress = strings.TrimSpace(address)
		os.Setenv("DBUS_SESSION_BUS_ADDRESS", sessionBusAddress)
	})

	if sessionBusErr != nil {
		t.Skipf("can't start a private dbus session bus: %v", sessionBusErr)
	}

	return sessionBusAddress
}

// connects to the private bus as a separate client, to play the part of gnome shell or kwin
func fakeServiceConnection(t *testing.T, address string, name string) *dbus.Conn {
	t.Helper()

	conn, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { conn.Close() })

	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("failed to own %s: %v", name, err)
	}

	return conn
}

type fakeGnomeWindows struct {
	windows string
}

func (f *fakeGnomeWindows) List() (string, *dbus.Error) {
	return f.windows, nil
}

func TestGnomeFocusProvider(t *testing.T) {
	address := privateSessionBus(t)
	provider := &gnomeFocusProvider{}

	// without the window calls extension, the user should be told how to fix it
	_, err := provider.activePID()

	var setupErr *FocusSetupError
	if !errors.As(err, &setupErr) {
		t.Fatalf("expected a FocusSetupError without the extension, got %v", err)
	}

	conn := fakeServiceConnection(t, address, "org.gnome.Shell")
	windows := &fakeGnomeWindows{
		windows: `[{"wm_class":"Terminal","pid":10,"focus":false},{"wm_class":"discord","pid":8080,"focus":true}]`,
	}

	if err := conn.Export(windows, "/org/gnome/Shell/Extensions/Windows", "org.gnome.Shell.Extensions.Windows"); err != nil {
		t.Fatal(err)
	}

	pid, err := provider.activePID()
	if err != nil {
		t.Fatal(err)
	}

	if pid != 8080 {
		t.Errorf("expected pid 8080, got %d", pid)
	}
}

// fakeKWin implements just enough of kwin's scripting interface: running a script makes it call back like
// the real script would, after a window gets activated
type fakeKWin struct {
	conn           *dbus.Conn
	scriptPath     string
	scriptName     string
	activePID      string
	unloadedScript string
}

var callDBusPattern = regexp.MustCompile(`callDBus\("([^"]+)", "([^"]+)", "([^"]+)", "Update"`)

func (k *fakeKWin) LoadScript(path string, name string) (int32, *dbus.Error) {
	k.scriptPath = path
	k.scriptName = name

	return 7, nil
}

func (k *fakeKWin) UnloadScript(name string) (bool, *dbus.Error) {
	k.unloadedScript = name
	return false, nil
}

func (k *fakeKWin) Run() *dbus.Error {
	script, err := ioutil.ReadFile(k.scriptPath)
	if err != nil {
		return dbus.MakeFailedError(err)
	}

	match := callDBusPattern.FindStringSubmatch(string(script))
	if match == nil {
		return dbus.MakeFailedError(fmt.Errorf("script doesn't call back over dbus"))
	}

	// kwin calls back asynchronously, once the script reports the active window
	go k.conn.Object(match[1], dbus.ObjectPath(match[2])).Call(match[3]+".Update", 0, k.activePID)

	return nil
}

func TestKwinFocusProvider(t *testing.T) {
	address := privateSessionBus(t)
	conn := fakeServiceConnection(t, address, "org.kde.KWin")

	kwin := &fakeKWin{conn: conn, activePID: "9001"}

	// kwin's methods are camelCase on the bus
	scripting := map[string]interface{}{"loadScript": kwin.LoadScript, "unloadScript": kwin.UnloadScript}
	if err := conn.ExportMethodTable(scripting, "/Scripting", "org.kde.kwin.Scripting"); err != nil {
		t.Fatal(err)
	}

	if err := conn.ExportMethodTable(map[string]interface{}{"run": kwin.Run}, "/Scripting/Script7", "org.kde.kwin.Script"); err != nil {
		t.Fatal(err)
	}

	provider := &kwinFocusProvider{}

	deadline := time.Now().Add(2 * time.Second)
	for {
		pid, err := provider.activePID()
		if err != nil {
			t.Fatal(err)
		}

		if pid == 9001 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("kwin script never reported the active window (last pid %d)", pid)
		}

		time.Sleep(20 * time.Millisecond)
	}

	if kwin.scriptName != kwinScriptPluginName || kwin.unloadedScript != kwinScriptPluginName {
		t.Errorf("expected leftover script to be unloaded before loading a new one, got %q/%q",
			kwin.unloadedScript, kwin.scriptName)
	}
}

type fakeFocusProvider struct {
	pid int
	err error
}

func (p *fakeFocusProvider) available() bool         { return true }
func (p *fakeFocusProvider) activePID() (int, error) { return p.pid, p.err }

func TestActiveWindowPIDProviderOrder(t *testing.T) {
	setupErr := &FocusSetupError{Message: "install the extension"}

	for name, testCase := range map[string]struct {
		providers []focusProvider
		pid       int
		err       error
	}{
		"first working provider wins": {
			providers: []focusProvider{&fakeFocusProvider{pid: 1}, &fakeFocusProvider{pid: 2}},
			pid:       1,
		},
		"falls back after an error": {
			providers: []focusProvider{&fakeFocusProvider{err: setupErr}, &fakeFocusProvider{pid: 2}},
			pid:       2,
		},
		"reports setup error when the fallback sees nothing focused": {
			providers: []focusProvider{&fakeFocusProvider{err: setupErr}, &fakeFocusProvider{pid: 0}},
			err:       setupErr,
		},
		"nothing focused is not an error": {
			providers: []focusProvider{&fakeFocusProvider{pid: 0}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			original := focusProviders
			focusProviders = testCase.providers
			defer func() { focusProviders = original }()

			pid, err := activeWindowPID()
			if pid != testCase.pid || err != testCase.err {
				t.Errorf("expected (%d, %v), got (%d, %v)", testCase.pid, testCase.err, pid, err)
			}
		})
	}
}

func TestProcessNamesIncludeDescendants(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("can't start a child process: %v", err)
	}

	defer child.Process.Kill()

	found := false
	for _, pid := range descendantPIDs(os.Getpid()) {
		if pid == child.Process.Pid {
			found = true
		}
	}

	if !found {
		t.Fatalf("expected child pid %d among descendants", child.Process.Pid)
	}

	names := processNames(child.Process.Pid)
	if len(names) == 0 || names[0] != "sleep" {
		t.Errorf("expected the child's names to start with \"sleep\", got %v", names)
	}
}
