package deej

import (
	"fyne.io/systray"

	"github.com/omriharel/deej/pkg/deej/icon"
	"github.com/omriharel/deej/pkg/deej/util"
)

func (d *Deej) initializeTray(onDone func()) {
	logger := d.logger.Named("tray")

	onReady := func() {
		logger.Debug("Tray instance ready")

		systray.SetTemplateIcon(icon.TrayLogo, icon.TrayLogo)
		systray.SetTitle("deej")
		systray.SetTooltip("deej")

		openEditor := systray.AddMenuItem("Open slider editor", "Map apps and devices to your sliders")
		openEditor.SetIcon(icon.TrayLogo)

		editConfig := systray.AddMenuItem("Edit configuration", "Open config file with a text editor")
		editConfig.SetIcon(icon.TrayEditConfig)

		refreshSessions := systray.AddMenuItem("Re-scan audio sessions", "Manually refresh audio sessions if something's stuck")
		refreshSessions.SetIcon(icon.TrayRefreshSessions)

		if d.version != "" {
			systray.AddSeparator()
			versionInfo := systray.AddMenuItem(d.version, "")
			versionInfo.Disable()
		}

		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop deej and quit")

		// wait on things to happen
		go func() {
			for {
				select {

				// quit
				case <-quit.ClickedCh:
					logger.Info("Quit menu item clicked, stopping")

					d.signalStop()

				// open slider editor
				case <-openEditor.ClickedCh:
					logger.Info("Open slider editor menu item clicked, opening it in the browser")

					d.editor.open()

				// edit config
				case <-editConfig.ClickedCh:
					logger.Info("Edit config menu item clicked, opening config for editing")

					// this may take a moment on linux, and shouldn't hold up the rest of the menu
					go func() {
						if err := util.OpenInTextEditor(logger, userConfigFilepath); err != nil {
							logger.Warnw("Failed to open config file for editing", "error", err)
							d.notifier.Notify("Can't open the config file",
								"No text editor found. Try the slider editor instead.")
						}
					}()

				// refresh sessions
				case <-refreshSessions.ClickedCh:
					logger.Info("Refresh sessions menu item clicked, triggering session map refresh")

					// performance: the reason that forcing a refresh here is okay is that users can't spam the
					// right-click -> select-this-option sequence at a rate that's meaningful to performance
					d.sessions.refreshSessions(true)
				}
			}
		}()

		// actually start the main runtime
		onDone()
	}

	onExit := func() {
		logger.Debug("Tray exited")
	}

	// start the tray icon
	logger.Debug("Running in tray")
	systray.Run(onReady, onExit)
}

func (d *Deej) stopTray() {
	d.logger.Debug("Quitting tray")
	systray.Quit()
}
