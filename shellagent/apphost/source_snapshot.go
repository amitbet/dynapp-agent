package apphost

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type sourceSnapshot struct {
	SchemaVersion int    `json:"schemaVersion"`
	Algorithm     string `json:"algorithm"`
	PackURL       string `json:"packUrl"`
	Files         []sourceSnapshotFile
}

type sourceSnapshotFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Mode   int    `json:"mode"`
}

// materializeSourceSnapshot downloads a Dyner source snapshot document,
// verifies it against expectedDigest (the revision's source_snapshot_digest),
// then fetches and verifies every blob. The bearer token in headers is only
// sent to URLs on the Dyner base origin.
func materializeSourceSnapshot(client *http.Client, sourceURL, expectedDigest, destination string, headers scopedHeaders) error {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequest(http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	copyHeaders(request.Header, headers.forURL(sourceURL))
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Dyner source snapshot download failed: HTTP %d", response.StatusCode)
	}
	document, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil {
		return err
	}
	if len(document) > 8<<20 {
		return errors.New("Dyner source snapshot is too large")
	}
	if !snapshotDigestMatches(expectedDigest, document) {
		return errors.New("Dyner source snapshot failed digest verification")
	}
	var snapshot sourceSnapshot
	if err := json.Unmarshal(document, &snapshot); err != nil {
		return errors.New("Dyner returned an invalid source snapshot")
	}
	if snapshot.SchemaVersion != 1 || snapshot.Algorithm != "sha256" || snapshot.PackURL == "" || len(snapshot.Files) == 0 {
		return errors.New("Dyner returned an invalid source snapshot")
	}
	expected := map[string]int64{}
	for _, file := range snapshot.Files {
		if err := validateSnapshotPath(file.Path); err != nil {
			return err
		}
		if !validSourceDigest(file.Digest) || file.Size < 0 {
			return errors.New("Dyner returned an unsafe source snapshot entry")
		}
		if previous, ok := expected[file.Digest]; ok && previous != file.Size {
			return fmt.Errorf("Dyner source snapshot has conflicting sizes for %s", file.Digest)
		}
		expected[file.Digest] = file.Size
	}
	packRequest, err := http.NewRequest(http.MethodPost, snapshot.PackURL, bytes.NewReader([]byte(`{"have":[]}`)))
	if err != nil {
		return err
	}
	copyHeaders(packRequest.Header, headers.forURL(snapshot.PackURL))
	packRequest.Header.Set("Content-Type", "application/json")
	packRequest.Header.Set("Accept", "application/vnd.dyner.source-pack")
	packResponse, err := client.Do(packRequest)
	if err != nil {
		return err
	}
	defer packResponse.Body.Close()
	if packResponse.StatusCode < 200 || packResponse.StatusCode >= 300 {
		return fmt.Errorf("Dyner source pack download failed: HTTP %d", packResponse.StatusCode)
	}
	packed, err := io.ReadAll(io.LimitReader(packResponse.Body, 256<<20))
	if err != nil {
		return err
	}
	if packResponse.Header.Get("Content-Encoding") == "gzip" || len(packed) >= 2 && packed[0] == 0x1f && packed[1] == 0x8b {
		reader, gzipErr := gzip.NewReader(bytes.NewReader(packed))
		if gzipErr != nil {
			return errors.New("Dyner returned an invalid source pack")
		}
		packed, err = io.ReadAll(io.LimitReader(reader, 256<<20))
		_ = reader.Close()
		if err != nil {
			return err
		}
	}
	objects, err := decodeSourcePack(packed)
	if err != nil {
		return err
	}
	root := filepath.Clean(destination)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	for _, file := range snapshot.Files {
		data, ok := objects[file.Digest]
		if !ok {
			return fmt.Errorf("Dyner source pack is missing object %s", file.Digest)
		}
		if int64(len(data)) != file.Size || sourceDigest(data) != file.Digest {
			return fmt.Errorf("Dyner source blob failed verification: %s", file.Path)
		}
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		if !pathInside(root, target) {
			return errors.New("Dyner source snapshot escapes the destination")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if file.Mode == 493 {
			mode = 0o755
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
	}
	return nil
}

// snapshotDigestMatches compares a `sha256:<hex>` (or bare hex) digest with
// the sha256 of the snapshot document bytes.
func snapshotDigestMatches(expected string, document []byte) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	expected = strings.TrimPrefix(expected, "sha256:")
	if len(expected) != 64 {
		return false
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return false
	}
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:]) == expected
}

func decodeSourcePack(pack []byte) (map[string][]byte, error) {
	if len(pack) < 12 || string(pack[:8]) != "DYNPACK1" {
		return nil, errors.New("Dyner returned an invalid source pack")
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	offset := 12
	objects := map[string][]byte{}
	for index := uint32(0); index < count; index++ {
		if offset+40 > len(pack) {
			return nil, errors.New("Dyner source pack ended unexpectedly")
		}
		digest := "sha256:" + hex.EncodeToString(pack[offset:offset+32])
		size := binary.BigEndian.Uint64(pack[offset+32 : offset+40])
		offset += 40
		if uint64(offset)+size > uint64(len(pack)) {
			return nil, errors.New("Dyner source pack ended unexpectedly")
		}
		data := append([]byte(nil), pack[offset:offset+int(size)]...)
		offset += int(size)
		if _, exists := objects[digest]; exists || sourceDigest(data) != digest {
			return nil, fmt.Errorf("Dyner source pack object failed verification: %s", digest)
		}
		objects[digest] = data
	}
	if offset != len(pack) {
		return nil, errors.New("Dyner source pack has trailing bytes")
	}
	return objects, nil
}

func encodeSourcePack(files []sourceSnapshotFile, blobs map[string][]byte) []byte {
	buffer := make([]byte, 12)
	copy(buffer, "DYNPACK1")
	binary.BigEndian.PutUint32(buffer[8:], uint32(len(files)))
	for _, file := range files {
		header := make([]byte, 40)
		raw, _ := hex.DecodeString(strings.TrimPrefix(file.Digest, "sha256:"))
		copy(header, raw)
		binary.BigEndian.PutUint64(header[32:], uint64(file.Size))
		buffer = append(buffer, header...)
		buffer = append(buffer, blobs[file.Digest]...)
	}
	return buffer
}

func sourceDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validSourceDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil
}

func validateSnapshotPath(portable string) error {
	if portable == "" || strings.HasPrefix(portable, "/") || strings.Contains(portable, `\`) {
		return errors.New("Dyner returned an unsafe source snapshot entry")
	}
	for _, part := range strings.Split(portable, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("Dyner returned an unsafe source snapshot entry")
		}
	}
	return nil
}

func pathInside(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
