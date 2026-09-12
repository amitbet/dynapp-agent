package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const updateHelperWait = time.Minute

var startUpdated = startUpdatedAgent
var stopUpdated = stopUpdatedService
var osExit = os.Exit
var waitAfterHelperStart = waitForHelperToTakeOver

func installedExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return resolveInstallPath(path)
}

func resolveInstallPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil
	}
	return resolved, nil
}

func cleanupStaleUpdateArtifacts(updateDir, executable string) {
	patterns := []string{
		filepath.Join(updateDir, ".dynapp-update-helper-*"),
		executable + ".prepared-*",
		executable + ".new-*",
		executable + ".old-*",
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}
}

func prepareReplacement(downloadedPath, executable string) (string, error) {
	preparedPath := executable + fmt.Sprintf(".prepared-%d", os.Getpid())
	if err := copyFile(downloadedPath, preparedPath, 0o700); err != nil {
		return "", fmt.Errorf("prepare replacement binary: %w", err)
	}
	return preparedPath, nil
}

func replaceExecutable(source, target string) (string, error) {
	if source == "" || target == "" {
		return "", errors.New("update source or target is missing")
	}
	if source == target {
		return "", errors.New("downloaded update has the same path as the running executable")
	}
	staged := source
	removeStaged := false
	if filepath.Dir(source) != filepath.Dir(target) {
		staged = target + fmt.Sprintf(".new-%d", os.Getpid())
		if err := copyFile(source, staged, 0o700); err != nil {
			return "", fmt.Errorf("copy downloaded agent: %w", err)
		}
		removeStaged = true
		defer func() {
			if removeStaged {
				_ = os.Remove(staged)
			}
		}()
	}
	backup := target + fmt.Sprintf(".old-%d", os.Getpid())
	if err := os.Rename(target, backup); err != nil {
		return "", fmt.Errorf("move old agent aside: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(backup, target)
		return "", fmt.Errorf("install new agent: %w", err)
	}
	removeStaged = false
	if staged != source {
		_ = os.Remove(source)
	}
	return backup, nil
}

func (p *program) scheduleHelperUpdate(downloadedPath, executable, version string) error {
	updateDir := filepath.Dir(downloadedPath)
	cleanupStaleUpdateArtifacts(updateDir, executable)
	preparedPath, err := prepareReplacement(downloadedPath, executable)
	if err != nil {
		return err
	}
	helperPath := filepath.Join(updateDir, fmt.Sprintf(".dynapp-update-helper-%d", os.Getpid()))
	if err := copyFile(executable, helperPath, 0o700); err != nil {
		_ = os.Remove(preparedPath)
		return fmt.Errorf("prepare update helper: %w", err)
	}
	restartArgs, err := json.Marshal(os.Args[1:])
	if err != nil {
		_ = os.Remove(preparedPath)
		_ = os.Remove(helperPath)
		return err
	}
	helperArgs := []string{
		"--update-helper",
		"--update-source", preparedPath,
		"--update-target", executable,
		"--update-parent", strconv.Itoa(os.Getpid()),
	}
	command := wrapHelperCommand(helperPath, helperArgs)
	command.Env = append(os.Environ(), "DYNAPP_UPDATE_RESTART_ARGS="+base64.RawStdEncoding.EncodeToString(restartArgs))
	if p.serviceMode {
		command.Env = append(command.Env, "DYNAPP_UPDATE_SERVICE=1")
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	detachUpdateHelper(command)
	if err := command.Start(); err != nil {
		_ = os.Remove(helperPath)
		_ = os.Remove(preparedPath)
		return fmt.Errorf("start update helper: %w", err)
	}
	if command.Process != nil {
		_ = command.Process.Release()
	}
	log.Printf("DynApp Shell agent: applying release %s", version)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownContext); err != nil {
		_ = os.Remove(preparedPath)
		return fmt.Errorf("stop agent for update: %w", err)
	}
	waitAfterHelperStart(p.serviceMode)
	return nil
}

func waitForHelperToTakeOver(serviceMode bool) {
	if !serviceMode {
		osExit(0)
		return
	}
	// Stay loaded so KeepAlive does not respawn the old binary. The helper
	// unloads/stops this job, replaces the file, then starts the new service.
	timer := time.NewTimer(2 * updateHelperWait)
	defer timer.Stop()
	<-timer.C
	log.Printf("DynApp Shell agent: update helper did not stop the service")
	osExit(1)
}

func runUpdateHelper(source, target string, parentPID int) error {
	if source == "" || target == "" || parentPID <= 0 {
		return errors.New("update helper arguments are incomplete")
	}
	if os.Getenv("DYNAPP_UPDATE_SERVICE") == "1" {
		if err := stopUpdated(target); err != nil {
			log.Printf("DynApp Shell agent: could not stop service before update: %v", err)
		}
	}
	if err := waitForProcessExit(parentPID, updateHelperWait); err != nil {
		return err
	}
	backup, err := replaceExecutable(source, target)
	if err != nil {
		return err
	}
	if err := startUpdated(target); err != nil {
		log.Printf("DynApp Shell agent: installed update; restart deferred: %v", err)
	}
	_ = os.Remove(backup)
	return nil
}

func startUpdatedAgent(target string) error {
	if os.Getenv("DYNAPP_UPDATE_SERVICE") == "1" {
		return controlUpdatedService(target, "start")
	}
	args := []string{}
	encoded := strings.TrimSpace(os.Getenv("DYNAPP_UPDATE_RESTART_ARGS"))
	if encoded != "" {
		data, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("decode restart arguments: %w", err)
		}
		if err := json.Unmarshal(data, &args); err != nil {
			return fmt.Errorf("decode restart arguments: %w", err)
		}
	}
	command := exec.Command(target, args...)
	command.Dir = filepath.Dir(target)
	command.Env = withoutEnv(os.Environ(), "DYNAPP_UPDATE_RESTART_ARGS")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start updated agent: %w", err)
	}
	return nil
}

func stopUpdatedService(target string) error {
	return controlUpdatedService(target, "stop")
}

func controlUpdatedService(target, action string) error {
	command := exec.Command(target, action)
	command.Dir = filepath.Dir(target)
	command.Env = withoutEnv(withoutEnv(os.Environ(), "DYNAPP_UPDATE_RESTART_ARGS"), "DYNAPP_UPDATE_SERVICE")
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.ToLower(string(output) + err.Error())
	if action == "start" && (strings.Contains(text, "already loaded") || strings.Contains(text, "already started") || strings.Contains(text, "already running")) {
		return nil
	}
	if action == "stop" && (strings.Contains(text, "not loaded") || strings.Contains(text, "not installed") || strings.Contains(text, "not running") || strings.Contains(text, "could not find")) {
		return nil
	}
	return fmt.Errorf("%s updated service: %w: %s", action, err, strings.TrimSpace(string(output)))
}

func copyFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Chmod(mode); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func withoutEnv(environment []string, name string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		if !strings.HasPrefix(value, prefix) {
			result = append(result, value)
		}
	}
	return result
}
