//go:build !linux && !nogui

package gui

import "github.com/wailsapp/wails/v3/pkg/application"

// setTrayTip: Wails' SetTooltip does it here.
func setTrayTip(*application.SystemTray, string) bool { return true }

// setTrayPassive: only Linux has Omarchy's bar to put the icon in instead.
func setTrayPassive(*application.SystemTray, bool) bool { return true }

// trayClick: Wails places the panel by the click itself here.
func trayClick(*application.SystemTray) (int, bool) { return 0, false }
