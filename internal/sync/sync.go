// Package sync ships an index between machines. Segment files are
// content-addressed and immutable, so a consumer only downloads what it lacks
// and the manifest is swapped last. Files travel zstd-compressed and land
// raw, since readers mmap them; each file's name carries the SHA-256 of its
// raw bytes, which pull checks after decompressing.
package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tfcace/marlex/internal/index"

	"github.com/klauspost/compress/zstd"
)

type Stats struct {
	Copied   []string
	Skipped  int
	Bytes    int64 // transferred (compressed)
	RawBytes int64 // as stored for mmap
}

// Sidecars selects the optional files to transfer with the core.
type Sidecars struct{ Blobs, Hunks bool }

var AllSidecars = Sidecars{Blobs: true, Hunks: true}

func files(m index.Manifest, side Sidecars) []string {
	var out []string
	for _, s := range m.Segments {
		out = append(out, s.File)
		if side.Blobs && s.Blobs != "" {
			out = append(out, s.Blobs)
		}
		if side.Hunks && s.Hunks != "" {
			out = append(out, s.Hunks)
		}
	}
	if m.Reach != "" {
		out = append(out, m.Reach)
	}
	return out
}

func readManifest(r io.Reader) ([]byte, index.Manifest, error) {
	var m index.Manifest
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, m, fmt.Errorf("manifest: %w", err)
	}
	for _, name := range files(m, AllSidecars) {
		if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".fb") {
			return nil, m, fmt.Errorf("manifest names an unsafe file %q", name)
		}
	}
	return data, m, nil
}

// Publish compresses an index directory into a destination directory (a
// mounted bucket, or a staging dir for `aws s3 sync` / `rclone`). Every
// file, sidecars included, is published; consumers choose what to pull.
func Publish(src, dst string) (*Stats, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(src, index.ManifestName))
	if err != nil {
		return nil, err
	}
	manifest, m, err := readManifest(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	st := &Stats{}
	for _, name := range files(m, AllSidecars) {
		out := filepath.Join(dst, name+".zst")
		if _, err := os.Stat(out); err == nil {
			st.Skipped++
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s is missing from the index; publish from the index that was built, not one that was pulled", name)
		}
		if err != nil {
			return nil, err
		}
		z := enc.EncodeAll(raw, nil)
		if _, err := writeAtomic(out, strings.NewReader(string(z))); err != nil {
			return nil, err
		}
		st.Copied = append(st.Copied, name)
		st.Bytes += int64(len(z))
		st.RawBytes += int64(len(raw))
	}
	_, err = writeAtomic(filepath.Join(dst, index.ManifestName), strings.NewReader(string(manifest)))
	return st, err
}

// Pull fetches an index from a directory or an http(s) base URL. The
// optional sidecars come along only when asked for.
func Pull(from, dst string, side Sidecars) (*Stats, error) {
	src := dirSource(from)
	if strings.HasPrefix(from, "http://") || strings.HasPrefix(from, "https://") {
		src = httpSource(strings.TrimSuffix(from, "/"))
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	rc, err := src(index.ManifestName)
	if err != nil {
		return nil, err
	}
	manifest, m, err := readManifest(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	st := &Stats{}
	for _, name := range files(m, side) {
		p := filepath.Join(dst, name)
		if _, err := os.Stat(p); err == nil {
			st.Skipped++
			continue
		}
		var body io.Reader
		rc, err := src(name + ".zst")
		compressed := err == nil
		if errors.Is(err, errNotFound) {
			rc, err = src(name) // a bucket published before compression
		}
		if err != nil {
			return nil, err
		}
		counted := &countingReader{r: rc}
		body = counted
		if compressed {
			if err := dec.Reset(counted); err != nil {
				rc.Close()
				return nil, err
			}
			body = dec
		}
		n, err := writeVerified(p, name, body)
		rc.Close()
		if err != nil {
			return nil, err
		}
		st.Copied = append(st.Copied, name)
		st.Bytes += counted.n
		st.RawBytes += n
	}
	if _, err = writeAtomic(filepath.Join(dst, index.ManifestName), strings.NewReader(string(manifest))); err != nil {
		return nil, err
	}
	// Files the new manifest no longer names (an older generation, or
	// commits a shallow clone indexed on top with mx ff) are dropped;
	// readers that have them open keep them until they close.
	keep := map[string]bool{}
	for _, name := range files(m, AllSidecars) {
		keep[name] = true
	}
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".fb") && !keep[n] {
			os.Remove(filepath.Join(dst, n))
		}
	}
	return st, nil
}

var errNotFound = errors.New("not found")

type source func(name string) (io.ReadCloser, error)

func dirSource(dir string) source {
	return func(name string) (io.ReadCloser, error) {
		f, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", name, errNotFound)
		}
		return f, err
	}
}

func httpSource(base string) source {
	return func(name string) (io.ReadCloser, error) {
		resp, err := http.Get(base + "/" + name)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return nil, fmt.Errorf("GET %s/%s: %w", base, name, errNotFound)
			}
			return nil, fmt.Errorf("GET %s/%s: %s", base, name, resp.Status)
		}
		return resp.Body, nil
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// writeVerified stores r at p only if its SHA-256 matches the digest prefix
// in the content-addressed name (e.g. seg-<24 hex>.fb).
func writeVerified(p, name string, r io.Reader) (int64, error) {
	stem := strings.TrimSuffix(name, ".fb")
	want := stem[strings.LastIndexByte(stem, '-')+1:]
	h := sha256.New()
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && !strings.HasPrefix(hex.EncodeToString(h.Sum(nil)), want) {
		err = fmt.Errorf("%s: content does not match its name (corrupt or tampered download)", name)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, p)
}

func writeAtomic(p string, r io.Reader) (int64, error) {
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, p)
}
