package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOriginVerifiesUploadWithoutKeepingWholeBody(t *testing.T) {
	data := bytes.Repeat([]byte("bounded"), 12000)
	r := httptest.NewRequest("POST", "/upload", bytes.NewReader(data))
	w := httptest.NewRecorder()
	handler(1<<20).ServeHTTP(w, r)
	var reply struct {
		Bytes  int
		SHA256 string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if w.Code != 200 || reply.Bytes != len(data) || reply.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal(w.Code, reply)
	}
	for _, length := range []int64{-1, 1025} {
		r = httptest.NewRequest("POST", "/upload", strings.NewReader("x"))
		r.ContentLength = length
		w = httptest.NewRecorder()
		handler(1024).ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("unbounded upload accepted")
		}
	}
	r = httptest.NewRequest("POST", "/upload", strings.NewReader("short"))
	r.ContentLength = 20
	w = httptest.NewRecorder()
	handler(1024).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("partial upload accepted")
	}
}

func TestOriginDownloadHasExactLengthAndKnownContent(t *testing.T) {
	server := httptest.NewServer(handler(8192))
	defer server.Close()
	r, err := http.Get(server.URL + "/bytes?n=4097")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil || r.ContentLength != 4097 || len(data) != 4097 || !bytes.Equal(data, make([]byte, 4097)) {
		t.Fatal("download mismatch", err)
	}
	w := httptest.NewRecorder()
	handler(8192).ServeHTTP(w, httptest.NewRequest("GET", "/bytes?n=8193", nil))
	if w.Code != 400 {
		t.Fatal("oversized download accepted")
	}
}
