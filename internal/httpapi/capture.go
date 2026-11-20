package httpapi

import (
	"bytes"
	"net/http"
)

type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *captureWriter) Header() http.Header { return w.header }

func (w *captureWriter) Write(body []byte) (int, error) { return w.body.Write(body) }

func (w *captureWriter) WriteHeader(status int) { w.status = status }
