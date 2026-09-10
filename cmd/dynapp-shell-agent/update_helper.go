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

func (p *program) scheduleSelfUpdate(_ context.Context, downloadedPath, version string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	if downloadedPath == executable {
		return errors.New("downloaded update has the same path as the running executable")
	}

	updateDir := filepath.Dir(downloadedPath)
	preparedPath := executable + fmt.Sprintf(".prepared-%d", os.Getpid())
	if err := copyFile(downloadedPath, preparedPath, 0o700); err != nil {
		return fmt.Errorf("prepare replacement binary: %w", err)
	}
	helperPath := filepath.Join(updateDir, fmt.Sprintf(".dynapp-update-helper-%d", os.Getpid()))
	if err := copyFile(executable, helperPath, 0o700); err != nil {
		_ = os.Remove(preparedPath)
		return fmt.Errorf("prepare update helper: %w", err)
	}
	restartArgs, err := json.Marshal(os.Args[1:])
	if err != nil {
		_ = os.Remove(preparedPath)
		return err
	}
	encodedArgs := base64.RawStdEncoding.EncodeToString(restartArgs)
	helperArgs := []string{
		"--update-helper",
		"--update-source", preparedPath,
		"--update-target", executable,
		"--update-parent", strconv.Itoa(os.Getpid()),
	}
	command := exec.Command(helperPath, helperArgs...)
	command.Env = append(os.Environ(), "DYNAPP_UPDATE_RESTART_ARGS="+encodedArgs)
	if p.serviceMode {
		command.Env = append(command.Env, "DYNAPP_UPDATE_SERVICE=1")
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = os.Remove(helperPath)
		_ = os.Remove(preparedPath)
		return fmt.Errorf("start update helper: %w", err)
	}
	log.Printf("DynApp Shell agent: applying release %s", version)
	// Shutdown cancels the updater context as part of normal cleanup. Use a
	// separate context here so that cleanup cannot cancel the shutdown itself.
	shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownContext); err != nil {
		_ = command.Process.Kill()
		_ = os.Remove(preparedPath)
		return fmt.Errorf("stop agent for update: %w", err)
	}
	// The helper owns the replacement and restart. Exiting here releases the
	// executable on Windows and keeps the downtime between processes short.
	os.Exit(0)
	return nil
}

func runUpdateHelper(source, target string, parentPID int) error {
	if source == "" || target == "" || parentPID <= 0 {
		return errors.New("update helper arguments are incomplete")
	}
	if err := waitForProcessExit(parentPID, updateHelperWait); err != nil {
		return err
	}
	staged := source
	if filepath.Dir(source) != filepath.Dir(target) {
		staged = target + fmt.Sprintf(".new-%d", os.Getpid())
	}
	defer os.Remove(staged)
	if staged != source {
		if err := copyFile(source, staged, 0o700); err != nil {
			return fmt.Errorf("copy downloaded agent: %w", err)
		}
	}
	backup := target + fmt.Sprintf(".old-%d", os.Getpid())
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("move old agent aside: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(backup, target)
		return fmt.Errorf("install new agent: %w", err)
	}
	if staged != source {
		_ = os.Remove(source)
	}
	if err := startUpdatedAgent(target); err != nil {
		_ = os.Remove(target)
		_ = os.Rename(backup, target)
		return err
	}
	_ = os.Remove(backup)
	return nil
}

func startUpdatedAgent(target string) error {
	if os.Getenv("DYNAPP_UPDATE_SERVICE") == "1" {
		return startUpdatedService(target)
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

func startUpdatedService(target string) error {
	command := exec.Command(target, "start")
	command.Dir = filepath.Dir(target)
	command.Env = withoutEnv(withoutEnv(os.Environ(), "DYNAPP_UPDATE_RESTART_ARGS"), "DYNAPP_UPDATE_SERVICE")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start updated service: %w", err)
	}
	return nil
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
