package icon

import (
	_ "embed" // needed for go:embed
)

// on linux, the tray icon is exposed over dbus (StatusNotifierItem) which only understands PNG data

// TrayLogo is the tray icon, as expected by the current platform
//
//go:embed png/logo.png
var TrayLogo []byte

// NotificationLogo is the icon shown alongside notifications, as expected by the current platform
//
//go:embed png/logo-256.png
var NotificationLogo []byte

// TrayEditConfig is the edit config menu item icon, as expected by the current platform
//
//go:embed png/edit-config.png
var TrayEditConfig []byte

// TrayRefreshSessions is the refresh sessions menu item icon, as expected by the current platform
//
//go:embed png/refresh-sessions.png
var TrayRefreshSessions []byte

// NotificationLogoExtension is the file extension matching NotificationLogo's format
const NotificationLogoExtension = ".png"
