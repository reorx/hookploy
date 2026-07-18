package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/reorx/hookploy/internal/ops"
)

// imageExtract: docker create → docker cp <from> → near-atomic swap into
// <to> → docker rm (always).
func (e *Engine) imageExtract(ctx context.Context, spec Spec, idx int, a *ops.ImageExtract, sink Sink) (*int, error) {
	img := a.Image
	if img == "" {
		if spec.Image == "" {
			return nil, fmt.Errorf("image.extract: no \"image\" arg and service declares no image")
		}
		img = spec.Image + ":latest"
	}
	if a.Pull {
		if exit, err := e.runCmd(ctx, spec, idx, sink, []string{"docker", "pull", img}); err != nil {
			return exit, err
		}
	}
	toPath, err := resolveWithin(spec.Dir, a.To)
	if err != nil {
		return nil, err
	}
	cid, err := e.runCapture(ctx, spec, idx, sink, []string{"docker", "create", img}, "stdout")
	if err != nil {
		return nil, fmt.Errorf("create temp container: %w", err)
	}
	defer func() {
		if _, rmErr := e.runCmd(ctx, spec, idx, sink, []string{"docker", "rm", "-f", cid}); rmErr != nil {
			sink.Log(idx, "system", fmt.Sprintf("warning: failed to remove temp container %s: %v\n", cid, rmErr))
		}
	}()

	newPath := toPath + ".new"
	if err := os.RemoveAll(newPath); err != nil {
		return nil, err
	}
	if exit, err := e.runCmd(ctx, spec, idx, sink,
		[]string{"docker", "cp", cid + ":" + a.From, newPath}); err != nil {
		return exit, fmt.Errorf("docker cp: %w", err)
	}
	if err := swapDir(toPath); err != nil {
		return nil, err
	}
	zero := 0
	return &zero, nil
}

// artifactExtract: download (retried) → streamed sha256 check → unpack to
// <to>.new → near-atomic swap.
func (e *Engine) artifactExtract(ctx context.Context, spec Spec, idx int, a *ops.ArtifactExtract, sink Sink) error {
	toPath, err := resolveWithin(spec.Dir, a.To)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(spec.Dir, ".artifact-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := e.download(ctx, idx, a.URL, tmp, a.SHA256, sink); err != nil {
		return err
	}

	newPath := toPath + ".new"
	if err := os.RemoveAll(newPath); err != nil {
		return err
	}
	kind := archiveKind(a.URL)
	switch kind {
	case "tar.gz":
		err = untarGz(tmp, newPath, sink, idx)
	case "zip":
		err = unzip(tmp, newPath)
	default:
		return fmt.Errorf("artifact.extract: unsupported archive type in %q (want .tar.gz/.tgz/.zip)", a.URL)
	}
	if err != nil {
		os.RemoveAll(newPath)
		return fmt.Errorf("unpack %s: %w", kind, err)
	}
	return swapDir(toPath)
}

// download fetches url into f (with retries) and verifies its sha256 while
// streaming.
func (e *Engine) download(ctx context.Context, idx int, rawURL string, f *os.File, wantSHA string, sink Sink) error {
	var lastErr error
	for attempt := 1; attempt <= e.downloadRetries(); attempt++ {
		lastErr = func() error {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := f.Truncate(0); err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				return err
			}
			resp, err := e.HTTP.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
			}
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
				return err
			}
			got := hex.EncodeToString(h.Sum(nil))
			if !strings.EqualFold(got, wantSHA) {
				return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA)
			}
			return nil
		}()
		if lastErr == nil {
			return nil
		}
		// A digest mismatch on a complete download will not fix itself.
		if strings.Contains(lastErr.Error(), "sha256 mismatch") {
			return lastErr
		}
		if attempt < e.downloadRetries() {
			sink.Log(idx, "system", fmt.Sprintf("download attempt %d/%d failed, retrying: %v\n", attempt, e.downloadRetries(), lastErr))
			if err := e.sleep(ctx, e.pullInterval()); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("download failed after %d attempts: %w", e.downloadRetries(), lastErr)
}

func archiveKind(rawURL string) string {
	p := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		p = u.Path
	}
	p = strings.ToLower(p)
	switch {
	case strings.HasSuffix(p, ".tar.gz"), strings.HasSuffix(p, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(p, ".zip"):
		return "zip"
	}
	return ""
}

func untarGz(f *os.File, dest string, sink Sink, idx int) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := resolveWithin(dest, hdr.Name) // entry must stay inside dest
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		default:
			sink.Log(idx, "system", fmt.Sprintf("skipping archive entry %q (type %c)\n", hdr.Name, hdr.Typeflag))
		}
	}
}

func unzip(f *os.File, dest string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, entry := range zr.File {
		target, err := resolveWithin(dest, entry.Name) // zip-slip check per entry
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, entry.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// swapDir installs <to>.new as <to> near-atomically:
// rm <to>.old → mv <to>→<to>.old → mv <to>.new→<to> → rm <to>.old.
func swapDir(to string) error {
	oldPath, newPath := to+".old", to+".new"
	if _, err := os.Stat(newPath); err != nil {
		return fmt.Errorf("swap: %s missing: %w", newPath, err)
	}
	if err := os.RemoveAll(oldPath); err != nil {
		return err
	}
	if _, err := os.Stat(to); err == nil {
		if err := os.Rename(to, oldPath); err != nil {
			return err
		}
	}
	if err := os.Rename(newPath, to); err != nil {
		// try to roll back the previous content
		if _, statErr := os.Stat(oldPath); statErr == nil {
			_ = os.Rename(oldPath, to)
		}
		return err
	}
	return os.RemoveAll(oldPath)
}
