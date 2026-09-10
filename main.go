package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent"
	"github.com/kardianos/service"
)

type program struct {
	server      *shellagent.Server
	started     chan error
	serviceMode bool
}

func (p *program) Start(service.Service) error {
	p.started = make(chan error, 1)
	go func() {
		err := p.server.ListenAndServe()
		p.started <- err
		if err != nil && err.Error() != "http: Server closed" {
			log.Printf("DynApp Shell agent stopped: %v", err)
		}
	}()
	// Listener setup happens before ListenAndServe blocks. Give immediate bind and
	// certificate errors a chance to reach service managers instead of leaving an
	// inert process behind.
	select {
	case err := <-p.started:
		return err
	case <-time.After(150 * time.Millisecond):
	}
	return nil
}
func (p *program) Stop(service.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
}

func configureLogging(stateDir string) (func(), error) {
	logDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(logDir, "agent.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	// stdout is intentional: some terminal hosts capture or hide stderr for a
	// long-running `go run` process. Always tee every standard-library log call
	// to the visible console and the service-owned persistent log.
	log.SetOutput(io.MultiWriter(os.Stdout, file))
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	return func() { _ = file.Close() }, nil
}

func availableLoopbackAddress(preferred string) (string, error) {
	host, port, err := net.SplitHostPort(preferred)
	if err != nil {
		return "", err
	}
	for candidate := port; ; candidate = "0" {
		udp, udpErr := net.ListenPacket("udp", net.JoinHostPort(host, candidate))
		if udpErr != nil {
			if candidate == "0" {
				return "", udpErr
			}
			continue
		}
		resolvedPort := udp.LocalAddr().(*net.UDPAddr).Port
		tcp, tcpErr := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(resolvedPort)))
		_ = udp.Close()
		if tcpErr == nil {
			_ = tcp.Close()
			return net.JoinHostPort(host, fmt.Sprint(resolvedPort)), nil
		}
		if candidate == "0" {
			return "", tcpErr
		}
	}
}

func startupAddress(preferred string, explicit, enableLAN bool, config shellagent.Config) string {
	if !explicit && !enableLAN && config.ListenerMode == shellagent.ListenerLocal && config.LANAddress != "" {
		return config.LANAddress
	}
	return preferred
}

func main() {
	address := flag.String("address", shellagent.DefaultAddress, "loopback HTTP address for health, settings UI, and launcher control")
	stateDir := flag.String("state-dir", "", "service-owned state directory")
	dynerURL := flag.String("dyner-url", "", "Dyner base URL (defaults to DYNER_BASE_URL or the packaged local Dyner)")
	credential := flag.String("device-credential", "", "optional existing device credential; omitted to create one like Electron")
	enableLAN := flag.Bool("enable-lan", false, "expose the WebTransport listener on the LAN using this hostname")
	relay := flag.Bool("relay", false, "enable the account-configured internet relay")
	lanAddress := flag.String("lan-address", "", "UDP bind address for LAN WebTransport (default 0.0.0.0:<port>)")
	appID := flag.String("app-id", "", "hosted app id for launcher/associations")
	appName := flag.String("app-name", "", "hosted app display name")
	appURL := flag.String("app-url", "", "hosted PWA URL")
	extensions := flag.String("extensions", "", "comma-separated file extensions")
	mcpEndpoint := flag.String("mcp-endpoint", "", "internal Claude MCP parent endpoint")
	mcpToken := flag.String("mcp-token", "", "internal Claude MCP bearer token")
	noSelfUpdate := flag.Bool("no-self-update", false, "disable hourly GitHub release checks")
	updateRepository := flag.String("update-repository", "", "GitHub owner/repository for agent updates")
	updateHelper := flag.Bool("update-helper", false, "run the private self-update helper")
	updateSource := flag.String("update-source", "", "downloaded binary for the self-update helper")
	updateTarget := flag.String("update-target", "", "installed binary for the self-update helper")
	updateParent := flag.Int("update-parent", 0, "old agent PID for the self-update helper")
	printVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *printVersion {
		fmt.Println(shellagent.AgentVersion)
		return
	}
	if len(flag.Args()) == 2 && flag.Arg(0) == "presentation-helper" {
		token := os.Getenv("DYNAPP_PRESENTATION_TOKEN")
		os.Unsetenv("DYNAPP_PRESENTATION_TOKEN")
		if err := shellagent.RunPresentationHelper(flag.Arg(1), token); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *updateHelper {
		if err := runUpdateHelper(*updateSource, *updateTarget, *updateParent); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "file-promise-helper" {
		if err := shellagent.RunFilePromiseHelper(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "mcp-proxy" {
		if err := shellagent.RunMCPProxyMain(*mcpEndpoint, *mcpToken); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *stateDir == "" {
		resolved, err := shellagent.DefaultStateDir()
		if err != nil {
			log.Fatal(err)
		}
		*stateDir = resolved
	}
	closeLog, err := configureLogging(*stateDir)
	if err != nil {
		log.Fatal(err)
	}
	defer closeLog()
	if *dynerURL == "" {
		*dynerURL = shellagent.DefaultDynerBaseURL()
	}
	if !*enableLAN {
		*enableLAN = shellagent.EnvEnabled("DYNAPP_ENABLE_LAN")
	}
	if len(flag.Args()) >= 1 && flag.Arg(0) == "launch-app" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shellagent.QueueExternalOpen(ctx, *address, *appID, flag.Args()[1:]); err != nil {
			if fallbackErr := shellagent.QueueExternalOpenFallback(*stateDir, *appID, flag.Args()[1:]); fallbackErr != nil {
				log.Printf("could not queue external open: %v (service: %v)", fallbackErr, err)
			}
		}
		if err := shellagent.OpenHostedApp(*appURL, *appName); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "install-associations" {
		executable, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		values := []string{}
		for _, value := range strings.Split(*extensions, ",") {
			value = strings.TrimPrefix(strings.TrimSpace(value), ".")
			if value != "" {
				values = append(values, value)
			}
		}
		if err := shellagent.InstallAssociations(shellagent.AssociationOptions{AppID: *appID, Name: *appName, URL: *appURL, Executable: executable, Extensions: values}); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "remove-associations" {
		if err := shellagent.RemoveAssociations(*appID); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "enroll" {
		config, err := shellagent.LoadConfig(*stateDir)
		if err != nil {
			log.Fatal(err)
		}
		config.DynerBaseURL = *dynerURL
		config.RelayEnabled = *relay
		if *credential != "" {
			parts := strings.SplitN(*credential, ".", 2)
			if len(parts) != 2 {
				log.Fatal("invalid device credential")
			}
			config.EnvironmentID = parts[0]
			config.DeviceCredential = *credential
		}
		config.ApplyListenerDefaults(*address, *enableLAN)
		if err := shellagent.SaveConfig(*stateDir, config); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "status" {
		config, err := shellagent.LoadConfig(*stateDir)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(config.Redacted()); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(flag.Args()) == 1 && flag.Arg(0) == "configure-lan" {
		config, err := shellagent.LoadConfig(*stateDir)
		if err != nil {
			log.Fatal(err)
		}
		if *lanAddress != "" {
			config.LANAddress = *lanAddress
		}
		config.ApplyListenerDefaults(*address, true)
		if err := shellagent.SaveConfig(*stateDir, config); err != nil {
			log.Fatal(err)
		}
		certificate, err := (&shellagent.Server{StateDir: *stateDir}).LANCertificateHash()
		if err != nil {
			log.Fatal(err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"enabled": true, "address": config.LANAddress, "name": config.ListenerName, "serverCertificateHash": certificate})
		return
	}
	config, err := shellagent.LoadConfig(*stateDir)
	if err != nil {
		log.Fatal(err)
	}
	if *dynerURL != "" {
		config.DynerBaseURL = *dynerURL
	}
	if *relay {
		config.RelayEnabled = true
	}
	if *lanAddress != "" {
		config.LANAddress = *lanAddress
	}
	if *credential != "" {
		parts := strings.SplitN(*credential, ".", 2)
		if len(parts) == 2 {
			config.EnvironmentID = parts[0]
			config.DeviceCredential = *credential
		}
	}
	addressExplicit := false
	flag.Visit(func(item *flag.Flag) { addressExplicit = addressExplicit || item.Name == "address" })
	preserveConfiguredLANAddress := !addressExplicit && !*enableLAN && *lanAddress == "" &&
		(config.ListenerMode == shellagent.ListenerLAN || (config.ListenerMode == "" && config.LANEnabled))
	configuredLANAddress := config.LANAddress
	*address = startupAddress(*address, addressExplicit, *enableLAN, config)
	if !addressExplicit && !*enableLAN && config.EnvironmentID == "" {
		available, listenErr := availableLoopbackAddress(*address)
		if listenErr != nil {
			log.Fatal(listenErr)
		}
		if available != *address {
			log.Printf("DynApp Shell agent: %s is occupied; using %s", *address, available)
			*address = available
		}
	}
	config.ApplyListenerDefaults(*address, *enableLAN)
	if preserveConfiguredLANAddress {
		config.LANAddress = configuredLANAddress
	}
	if *lanAddress != "" {
		config.LANAddress = *lanAddress
		config.ListenerMode = shellagent.ListenerLAN
		config.LANEnabled = true
	}
	if err := shellagent.SaveConfig(*stateDir, config); err != nil {
		log.Fatal(err)
	}
	accountToken, err := shellagent.ResolveAccountToken("", "")
	if err != nil {
		log.Printf("DynApp Shell agent: could not read Dyner account: %v", err)
	}
	p := &program{server: &shellagent.Server{Address: *address, StateDir: *stateDir, Config: config, AccountToken: accountToken}}
	selfUpdate := shellagent.DefaultSelfUpdateConfig()
	if *noSelfUpdate {
		selfUpdate.Enabled = false
	}
	if strings.TrimSpace(*updateRepository) != "" {
		selfUpdate.Repository = strings.TrimSpace(*updateRepository)
	}
	selfUpdate.OnUpdate = p.scheduleSelfUpdate
	p.server.SelfUpdate = selfUpdate
	serviceOptions := service.KeyValue{}
	if runtime.GOOS != "windows" {
		serviceOptions["UserService"] = true
	}
	serviceConfig := &service.Config{
		Name: "dynapp-shell-agent", DisplayName: "DynApp Agent",
		Description: "Gives DynApps local access to this machine",
		Arguments:   []string{"--address", *address, "--state-dir", *stateDir},
		Option:      serviceOptions,
	}
	if *enableLAN {
		serviceConfig.Arguments = append(serviceConfig.Arguments, "--enable-lan")
	}
	if config.ListenerMode == shellagent.ListenerLAN {
		serviceConfig.Arguments = append(serviceConfig.Arguments, "--lan-address", config.LANAddress)
	}
	if *noSelfUpdate {
		serviceConfig.Arguments = append(serviceConfig.Arguments, "--no-self-update")
	}
	if strings.TrimSpace(*updateRepository) != "" {
		serviceConfig.Arguments = append(serviceConfig.Arguments, "--update-repository", strings.TrimSpace(*updateRepository))
	}
	svc, err := service.New(p, serviceConfig)
	if err != nil {
		log.Fatal(err)
	}
	p.serviceMode = !service.Interactive()
	if len(flag.Args()) == 1 {
		switch flag.Arg(0) {
		case "install":
			if err := service.Control(svc, flag.Arg(0)); err != nil {
				log.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				_ = service.Control(svc, "uninstall")
				log.Fatal(err)
			}
			if err := provisionServiceFirewall(config, executable); err != nil {
				_ = service.Control(svc, "uninstall")
				log.Fatal(err)
			}
			return
		case "uninstall":
			executable, err := os.Executable()
			serviceErr := service.Control(svc, flag.Arg(0))
			if err == nil {
				if firewallErr := removeServiceFirewall(config, executable); firewallErr != nil {
					log.Printf("DynApp Shell agent: could not remove firewall rule: %v", firewallErr)
				}
			} else {
				log.Printf("DynApp Shell agent: could not remove firewall rule: %v", err)
			}
			if serviceErr != nil {
				log.Fatal(serviceErr)
			}
			return
		case "start", "stop", "restart":
			if err := service.Control(svc, flag.Arg(0)); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	log.Printf("DynApp Shell agent listening on %s · WebTransport %s (%s) · settings http://%s/", *address, config.LANAddress, config.ListenerMode, *address)
	if service.Interactive() || os.Getenv("DYNAPP_AGENT_FOREGROUND") == "1" {
		if err := p.server.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := svc.Run(); err != nil {
		log.Fatal(err)
	}
}
