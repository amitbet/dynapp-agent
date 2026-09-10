package shellagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type portListener struct {
	Address     string `json:"address"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	PID         *int   `json:"pid"`
	ProcessName string `json:"processName"`
	Executable  string `json:"executable"`
}

func (s *Server) handleSystemRPC(ctx context.Context, request message) (any, error) {
	switch request.Method {
	case "listPorts":
		return listSystemPorts(ctx)
	case "terminate":
		pid := int(integer64(firstArg(request.Args)))
		if pid < 1 {
			return nil, errors.New("a valid process id is required")
		}
		if pid == os.Getpid() {
			return nil, errors.New("DynApp cannot terminate its own shell process")
		}
		listeners, err := listSystemPorts(ctx)
		if err != nil {
			return nil, err
		}
		found := false
		for _, listener := range listeners {
			if listener.PID != nil && *listener.PID == pid {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("process %d is no longer listening on a port", pid)
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			return nil, err
		}
		if err = process.Kill(); err != nil {
			return nil, err
		}
		return map[string]any{"pid": pid, "signal": "SIGKILL"}, nil
	default:
		return nil, errors.New("unsupported system method")
	}
}
func listSystemPorts(ctx context.Context) ([]portListener, error) {
	if runtime.GOOS == "windows" {
		return listWindowsPorts(ctx)
	}
	output, err := exec.CommandContext(ctx, "lsof", "-nP", "-FpcPnT", "-iTCP", "-sTCP:LISTEN", "-iUDP").Output()
	if err == nil {
		return parseLsofPorts(string(output)), nil
	}
	if runtime.GOOS != "linux" {
		return nil, err
	}
	output, err = exec.CommandContext(ctx, "ss", "-H", "-lntup").Output()
	if err != nil {
		return nil, err
	}
	return parseSSPorts(string(output)), nil
}
func parseEndpoint(value string) (string, int, bool) {
	value = strings.Split(value, "->")[0]
	index := strings.LastIndex(value, ":")
	if index < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(value[index+1:])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	address := strings.Trim(value[:index], "[]")
	if address == "" {
		address = "*"
	}
	return address, port, true
}
func parseLsofPorts(output string) []portListener {
	result := []portListener{}
	pid := 0
	name := ""
	protocol := ""
	endpoint := ""
	flush := func() {
		address, port, ok := parseEndpoint(endpoint)
		if ok && pid > 0 && !strings.Contains(endpoint, "->") {
			copyPID := pid
			result = append(result, portListener{Address: address, Port: port, Protocol: strings.ToLower(protocol), PID: &copyPID, ProcessName: name})
		}
		protocol, endpoint = "", ""
	}
	for scanner := bufio.NewScanner(strings.NewReader(output)); scanner.Scan(); {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
			pid, _ = strconv.Atoi(line[1:])
		case 'c':
			name = line[1:]
		case 'f':
			flush()
		case 'P':
			protocol = line[1:]
		case 'n':
			endpoint = line[1:]
		}
	}
	flush()
	return uniquePorts(result)
}

var ssOwnerPattern = regexp.MustCompile(`"([^"]+)",pid=(\d+)`)

func parseSSPorts(output string) []portListener {
	result := []portListener{}
	for scanner := bufio.NewScanner(strings.NewReader(output)); scanner.Scan(); {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		protocol, state := strings.ToLower(fields[0]), strings.ToUpper(fields[1])
		if protocol != "tcp" && protocol != "udp" || protocol == "tcp" && state != "LISTEN" {
			continue
		}
		address, port, ok := parseEndpoint(fields[4])
		if !ok {
			continue
		}
		matches := ssOwnerPattern.FindAllStringSubmatch(scanner.Text(), -1)
		if len(matches) == 0 {
			result = append(result, portListener{Address: address, Port: port, Protocol: protocol, ProcessName: "Unavailable"})
		}
		for _, match := range matches {
			pid, _ := strconv.Atoi(match[2])
			copyPID := pid
			result = append(result, portListener{Address: address, Port: port, Protocol: protocol, PID: &copyPID, ProcessName: match[1]})
		}
	}
	return uniquePorts(result)
}
func uniquePorts(values []portListener) []portListener {
	seen := map[string]bool{}
	result := []portListener{}
	for _, value := range values {
		pid := "unknown"
		if value.PID != nil {
			pid = strconv.Itoa(*value.PID)
		}
		key := fmt.Sprintf("%s:%s:%d:%s", value.Protocol, value.Address, value.Port, pid)
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Port < result[j].Port })
	return result
}
func listWindowsPorts(ctx context.Context) ([]portListener, error) {
	script := `$tcp=Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue|%{[pscustomobject]@{address=$_.LocalAddress;port=$_.LocalPort;protocol='tcp';pid=$_.OwningProcess}};$udp=Get-NetUDPEndpoint -ErrorAction SilentlyContinue|%{[pscustomobject]@{address=$_.LocalAddress;port=$_.LocalPort;protocol='udp';pid=$_.OwningProcess}};@($tcp)+@($udp)|%{$p=Get-Process -Id $_.pid -ErrorAction SilentlyContinue;$_|Add-Member processName $p.ProcessName -PassThru|Add-Member executable $p.Path -PassThru}|ConvertTo-Json -Compress`
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return nil, err
	}
	var raw any
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, err
	}
	items := []any{}
	if array, ok := raw.([]any); ok {
		items = array
	} else {
		items = []any{raw}
	}
	result := []portListener{}
	for _, item := range items {
		record, _ := item.(map[string]any)
		pid, port := int(integer64(record["pid"])), int(integer64(record["port"]))
		if pid < 1 || port < 1 {
			continue
		}
		copyPID := pid
		result = append(result, portListener{Address: stringValue(record["address"]), Port: port, Protocol: stringValue(record["protocol"]), PID: &copyPID, ProcessName: stringValue(record["processName"]), Executable: stringValue(record["executable"])})
	}
	return uniquePorts(result), nil
}
