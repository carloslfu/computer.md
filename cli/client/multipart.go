// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"path/filepath"
)

// Tiny wrapper around mime/multipart so callers in client.go can build
// uploads without dragging the multipart vocabulary through every signature.

type multipartWriter struct {
	w *multipart.Writer
}

func newMultipartWriter(buf *bytes.Buffer) *multipartWriter {
	return &multipartWriter{w: multipart.NewWriter(buf)}
}

func (m *multipartWriter) writeField(name, value string) error {
	return m.w.WriteField(name, value)
}

func (m *multipartWriter) writeFile(field, filename string, r io.Reader) error {
	fw, err := m.w.CreateFormFile(field, filepath.Base(filename))
	if err != nil {
		return fmt.Errorf("creating form file: %w", err)
	}
	if _, err := io.Copy(fw, r); err != nil {
		return fmt.Errorf("copying file: %w", err)
	}
	return nil
}

func (m *multipartWriter) close() error {
	return m.w.Close()
}

func (m *multipartWriter) contentType() string {
	return m.w.FormDataContentType()
}
