package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type trayItem struct {
	ID      string     `json:"id,omitempty"`
	Label   string     `json:"label,omitempty"`
	Type    string     `json:"type,omitempty"`
	Enabled *bool      `json:"enabled,omitempty"`
	Checked bool       `json:"checked,omitempty"`
	Submenu []trayItem `json:"submenu,omitempty"`
}
type trayOptions struct {
	Tooltip string     `json:"tooltip"`
	Title   string     `json:"title"`
	Menu    []trayItem `json:"menu"`
}
type presentationKey struct {
	Code      uint32
	Modifiers uint32
}

// Modifiers use Win32 values, translated to Carbon flags by the macOS adapter.
func parsePresentationKey(accelerator, platform string) (presentationKey, error) {
	var result presentationKey
	parts := strings.Split(accelerator, "+")
	if len(parts) < 2 || len(accelerator) > 128 {
		return result, errors.New("shortcut must contain a modifier and one key")
	}
	for _, part := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "alt", "option":
			result.Modifiers |= 1
		case "ctrl", "control":
			result.Modifiers |= 2
		case "shift":
			result.Modifiers |= 4
		case "cmd", "command", "super", "meta":
			result.Modifiers |= 8
		case "commandorcontrol", "cmdorctrl":
			if platform == "darwin" {
				result.Modifiers |= 8
			} else {
				result.Modifiers |= 2
			}
		default:
			return result, fmt.Errorf("unsupported shortcut modifier: %s", part)
		}
	}
	key := strings.ToUpper(strings.TrimSpace(parts[len(parts)-1]))
	if len(key) == 1 && ((key[0] >= 'A' && key[0] <= 'Z') || (key[0] >= '0' && key[0] <= '9')) {
		result.Code = uint32(key[0])
	} else {
		keys := map[string]uint32{"SPACE": 32, "TAB": 9, "ENTER": 13, "RETURN": 13, "ESC": 27, "ESCAPE": 27, "BACKSPACE": 8, "DELETE": 46, "INSERT": 45, "HOME": 36, "END": 35, "PAGEUP": 33, "PAGEDOWN": 34, "LEFT": 37, "UP": 38, "RIGHT": 39, "DOWN": 40, "PLUS": 187, "MINUS": 189, "COMMA": 188, "PERIOD": 190}
		result.Code = keys[key]
		if strings.HasPrefix(key, "F") {
			n, _ := strconv.Atoi(key[1:])
			if n >= 1 && n <= 20 {
				result.Code = uint32(111 + n)
			}
		}
	}
	if result.Code == 0 {
		return result, fmt.Errorf("unsupported shortcut key: %s", key)
	}
	return result, nil
}

func decodePresentationArg(args []any, target any) error {
	if len(args) != 1 {
		return errors.New("expected one argument")
	}
	data, err := json.Marshal(args[0])
	if err != nil {
		return err
	}
	if len(data) > 64*1024 {
		return errors.New("presentation argument exceeds 64 KiB")
	}
	return json.Unmarshal(data, target)
}
func validateTray(options trayOptions) error {
	if len(options.Tooltip) > 1024 || len(options.Title) > 256 {
		return errors.New("tray text is too long")
	}
	count := 0
	ids := map[string]bool{}
	var visit func([]trayItem, int) error
	visit = func(items []trayItem, depth int) error {
		if depth > 5 {
			return errors.New("tray menu is too deeply nested")
		}
		for _, item := range items {
			count++
			if count > 128 {
				return errors.New("tray menu exceeds 128 items")
			}
			if len(item.Label) > 512 || len(item.ID) > 128 {
				return errors.New("tray menu text is too long")
			}
			if item.Type != "" && item.Type != "normal" && item.Type != "separator" && item.Type != "checkbox" && item.Type != "radio" {
				return errors.New("unsupported tray menu item type")
			}
			if item.ID != "" {
				if ids[item.ID] {
					return errors.New("tray menu ids must be unique")
				}
				ids[item.ID] = true
			}
			if err := visit(item.Submenu, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(options.Menu, 0)
}

type presentationNative interface {
	setTray(trayOptions) error
	destroyTray()
	balloon(string, string) bool
	register(uint32, presentationKey) bool
	unregister(uint32)
}
type presentationController struct {
	native    presentationNative
	platform  string
	tray      *trayOptions
	shortcuts map[string]uint32
	keys      map[presentationKey]string
	nextID    uint32
}

func (p *presentationController) close() {
	p.native.destroyTray()
	for _, id := range p.shortcuts {
		p.native.unregister(id)
	}
}
func (p *presentationController) handle(command presentationCommand) (any, error) {
	if command.Service == "screen" {
		native, ok := p.native.(screenCaptureNative)
		if !ok {
			return nil, errors.New("native screenshots are unavailable")
		}
		return handleScreenCapture(native, command)
	}
	if p.shortcuts == nil {
		p.shortcuts = map[string]uint32{}
		p.keys = map[presentationKey]string{}
	}
	if command.Service == "globalShortcut" {
		if command.Method == "unregisterAll" {
			for _, id := range p.shortcuts {
				p.native.unregister(id)
			}
			clear(p.shortcuts)
			clear(p.keys)
			return true, nil
		}
		var accelerator string
		if err := decodePresentationArg(command.Args, &accelerator); err != nil {
			return nil, err
		}
		key, err := parsePresentationKey(accelerator, p.platform)
		if err != nil {
			return nil, err
		}
		switch command.Method {
		case "register":
			if _, ok := p.keys[key]; ok {
				return false, nil
			}
			if len(p.shortcuts) >= 64 {
				return nil, errors.New("an app may register at most 64 shortcuts")
			}
			p.nextID++
			if p.nextID > 0xbfff {
				return nil, errors.New("shortcut id limit reached; reconnect the app")
			}
			if !p.native.register(p.nextID, key) {
				return false, nil
			}
			p.shortcuts[accelerator] = p.nextID
			p.keys[key] = accelerator
			return true, nil
		case "unregister":
			if original, ok := p.keys[key]; ok {
				p.native.unregister(p.shortcuts[original])
				delete(p.shortcuts, original)
				delete(p.keys, key)
			}
			return true, nil
		}
	} else if command.Service == "tray" {
		if command.Method == "destroy" {
			p.native.destroyTray()
			p.tray = nil
			return true, nil
		}
		if command.Method == "displayBalloon" {
			var options struct {
				Title   string `json:"title"`
				Content string `json:"content"`
			}
			if err := decodePresentationArg(command.Args, &options); err != nil {
				return nil, err
			}
			if p.tray == nil {
				return false, nil
			}
			return p.native.balloon(options.Title, options.Content), nil
		}
		var options trayOptions
		if p.tray != nil {
			options = *p.tray
		}
		switch command.Method {
		case "create":
			options = trayOptions{}
			if len(command.Args) > 0 {
				if err := decodePresentationArg(command.Args, &options); err != nil {
					return nil, err
				}
			}
		case "setToolTip":
			if err := decodePresentationArg(command.Args, &options.Tooltip); err != nil {
				return nil, err
			}
		case "setTitle":
			if err := decodePresentationArg(command.Args, &options.Title); err != nil {
				return nil, err
			}
		case "setMenu":
			var menu []trayItem
			if err := decodePresentationArg(command.Args, &menu); err != nil {
				return nil, err
			}
			options.Menu = menu
		default:
			return nil, errors.New("unsupported tray method")
		}
		if err := validateTray(options); err != nil {
			return nil, err
		}
		if command.Method != "create" && p.tray == nil {
			return true, nil
		}
		if err := p.native.setTray(options); err != nil {
			return nil, err
		}
		p.tray = &options
		return true, nil
	}
	return nil, errors.New("unsupported presentation method")
}
func (p *presentationController) hotkey(id uint32) any {
	for accelerator, registered := range p.shortcuts {
		if registered == id {
			return map[string]any{"accelerator": accelerator}
		}
	}
	return nil
}
func (p *presentationController) menuClick(id string) any {
	if p.tray == nil {
		return nil
	}
	var visit func([]trayItem) any
	visit = func(items []trayItem) any {
		for i := range items {
			item := &items[i]
			if item.ID == id && id != "" && (item.Enabled == nil || *item.Enabled) {
				if item.Type == "checkbox" {
					item.Checked = !item.Checked
				}
				if item.Type == "radio" {
					for j := i; j >= 0 && items[j].Type == "radio"; j-- {
						items[j].Checked = j == i
					}
					for j := i + 1; j < len(items) && items[j].Type == "radio"; j++ {
						items[j].Checked = false
					}
				}
				return map[string]any{"id": id, "checked": item.Checked}
			}
			if event := visit(item.Submenu); event != nil {
				return event
			}
		}
		return nil
	}
	return visit(p.tray.Menu)
}
