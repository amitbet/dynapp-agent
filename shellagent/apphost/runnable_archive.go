package apphost

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxRunnableArchiveBytes = 256 << 20
	maxRunnableContentBytes = 512 << 20
	maxRunnableEntries      = 20_000
)

// materializeRunnableArchive downloads and unpacks a runnable zip. When the
// revision carries runnable_sha256 the archive must match it.
func materializeRunnableArchive(client *http.Client, runnableURL, expectedSHA256, destination string, headers scopedHeaders) error {
	request, err := http.NewRequest(http.MethodGet, runnableURL, nil)
	if err != nil {
		return err
	}
	copyHeaders(request.Header, headers.forURL(runnableURL))
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Dyner runnable download failed: HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxRunnableArchiveBytes+1))
	if err != nil {
		return err
	}
	if len(archive) > maxRunnableArchiveBytes {
		return errors.New("Dyner runnable archive is too large")
	}
	if expected := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(expectedSHA256), "sha256:")); expected != "" {
		sum := sha256.Sum256(archive)
		if hex.EncodeToString(sum[:]) != expected {
			return errors.New("Dyner runnable archive failed sha256 verification")
		}
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxRunnableEntries {
		return errors.New("Dyner returned an invalid runnable archive")
	}
	parent := filepath.Dir(destination)
	staging, err := os.MkdirTemp(parent, ".runnable-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	var total uint64
	for _, entry := range reader.File {
		portable := strings.TrimSuffix(entry.Name, "/")
		if portable == "" && entry.FileInfo().IsDir() {
			continue
		}
		if err := validateSnapshotPath(portable); err != nil || entry.Mode()&os.ModeType != 0 && !entry.FileInfo().IsDir() {
			return errors.New("Dyner returned an unsafe runnable archive entry")
		}
		total += entry.UncompressedSize64
		if total > maxRunnableContentBytes {
			return errors.New("Dyner runnable content is too large")
		}
		target := filepath.Join(staging, filepath.FromSlash(portable))
		if !pathInside(staging, target) {
			return errors.New("Dyner runnable archive escapes the destination")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		closeErr := input.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if uint64(len(data)) != entry.UncompressedSize64 {
			return errors.New("Dyner runnable archive entry has an invalid size")
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(staging, "index.html")); err != nil {
		return errors.New("Dyner runnable archive has no content/index.html")
	}
	if err := os.RemoveAll(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(staging, destination)
}
