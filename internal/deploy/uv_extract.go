package deploy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// extractUVBinary pulls the uv executable out of its release archive.
//
// The archive holds a single top-level directory containing uv and uvx. Only
// the regular-file entries named uv are considered, and the destination is
// always inside dest, so an archive with an unexpected layout cannot write
// outside the extraction directory.
func extractUVBinary(archive, dest string) (string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("read uv archive: %w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read uv archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(header.Name) != "uv" {
			continue
		}

		target := filepath.Join(dest, "uv")
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, reader); err != nil {
			out.Close()
			return "", fmt.Errorf("extract uv: %w", err)
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", fmt.Errorf("uv archive %s contains no uv executable", filepath.Base(archive))
}
